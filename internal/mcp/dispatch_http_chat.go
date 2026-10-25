package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// All chat hops, including target lookup and read marking, use executeRequest
// so the same authentication, scope and verified-email checks apply to each.
func (d *HTTPHandlerDispatcher) chatRequest(ctx context.Context, key string, user *models.User, method, path string, body any) *CallToolResult {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return dispatcherErrorResult(key, "encode request", err)
		}
	}
	res, err := d.executeRequest(ctx, key, user, method, path, raw)
	if err != nil {
		return dispatcherErrorResult(key, "execute request", err)
	}
	return res
}

// The decoder is deliberately chat-specific: these responses contain no
// collection definitions or item field schemas.
type chatHTTPResponse struct {
	ReadSeq       int64 `json:"read_seq"`
	Conversations []struct {
		Number   int    `json:"number"`
		AgentKey string `json:"agent_key"`
	} `json:"conversations"`
	Conversation struct {
		Number int `json:"number"`
	} `json:"conversation"`
	Messages []struct {
		Seq int64 `json:"seq"`
	} `json:"messages"`
}

func decodeChatResult(res *CallToolResult) (*chatHTTPResponse, error) {
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, err
	}
	var response chatHTTPResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (d *HTTPHandlerDispatcher) resolveChatTarget(ctx context.Context, key string, input map[string]any, user *models.User, ensure bool) (int, *CallToolResult) {
	target, _ := input["target"].(string)
	n, agent, err := cli.ParseChatTarget(target)
	if err != nil {
		return 0, validationFailedResult(key, err.Error(), "Use C<n>, <n>, or @<name>.")
	}
	if agent == "" {
		return n, nil
	}
	res := d.chatRequest(ctx, key, user, http.MethodGet, "/api/v1/me/conversations", nil)
	if res.IsError {
		return 0, res
	}
	list, err := decodeChatResult(res)
	if err != nil {
		return 0, dispatcherErrorResult(key, "decode conversations", err)
	}
	for _, c := range list.Conversations {
		if c.AgentKey == agent {
			return c.Number, nil
		}
	}
	if !ensure {
		return 0, NewErrorResult(ErrorPayload{Code: ErrNotFound, Message: fmt.Sprintf("Conversation for @%s not found", agent), Hint: "Use pad_chat list to find an existing conversation."})
	}
	res = d.chatRequest(ctx, key, user, http.MethodPost, "/api/v1/me/conversations", map[string]string{"agent_key": agent})
	if res.IsError {
		return 0, res
	}
	result, err := decodeChatResult(res)
	if err != nil {
		return 0, dispatcherErrorResult(key, "decode conversation", err)
	}
	return result.Conversation.Number, nil
}

func chatSequence(input map[string]any, name string, min int64) (int64, error) {
	v, ok := input[name]
	if !ok {
		return 0, fmt.Errorf("%s is required", name)
	}
	// BUG-4065: numericInput's one rule, so a float64 gets the exclusive 2^53
	// bound (above it a seq could be forwarded as its rounded neighbour), a
	// json.Number is exact ("1e6" is 1000000), and a string is admitted only as
	// a plain decimal integer. A present null is refused, as before. The
	// message is unchanged.
	n, present, err := numericInput(v)
	if err != nil || !present || n < min {
		return 0, fmt.Errorf("%s must be an integer of at least %d", name, min)
	}
	return n, nil
}

func (d *HTTPHandlerDispatcher) dispatchChat(verb string) specialDispatchFn {
	return func(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
		key := "chat " + verb
		invalid := func(err error) (*CallToolResult, error) {
			return validationFailedResult(key, err.Error(), "Check the action's required and optional parameters."), nil
		}
		// Validate before target resolution can ensure a DM.
		var seq int64
		var in cli.ChatSendInput
		q := url.Values{}
		q.Set("limit", "50")
		noMark := false
		switch verb {
		case "show":
			for _, field := range []struct {
				name, query string
				min         int64
			}{{"since", "after_seq", 0}, {"limit", "limit", 1}} {
				if _, ok := input[field.name]; ok {
					n, err := chatSequence(input, field.name, field.min)
					if err != nil {
						return invalid(err)
					}
					if field.name == "limit" && n > 200 {
						return invalid(fmt.Errorf("limit must be between 1 and 200"))
					}
					q.Set(field.query, strconv.FormatInt(n, 10))
				}
			}
			if v, ok := input["no_mark"]; ok {
				var err error
				noMark, err = toBool(v)
				if err != nil {
					return invalid(err)
				}
			}
		case "send":
			body, ok := input["body"].(string)
			if !ok {
				return invalid(fmt.Errorf("body is required"))
			}
			if body == "-" {
				return invalid(errors.New(chatStdinBodyError))
			}
			in.Body = body
			if _, ok := input["reply_to"]; ok {
				n, err := chatSequence(input, "reply_to", 1)
				if err != nil {
					return invalid(err)
				}
				in.ReplyToSeq = &n
			}
			if v, ok := input["op_key"]; ok {
				value, ok := v.(string)
				if !ok {
					return invalid(fmt.Errorf("op_key must be a string"))
				}
				in.OpKey = &value
			}
			in.ContextWorkspace, _ = input["context_workspace"].(string)
		case "delete", "read":
			field, min := "seq", int64(1)
			if verb == "read" {
				field, min = "to", 0
			}
			var err error
			seq, err = chatSequence(input, field, min)
			if err != nil {
				return invalid(err)
			}
		}
		n, failure := d.resolveChatTarget(ctx, key, input, user, verb == "send")
		if failure != nil {
			return failure, nil
		}
		path := fmt.Sprintf("/api/v1/me/conversations/%d", n)
		switch verb {
		case "show":
			res := d.chatRequest(ctx, key, user, http.MethodGet, path+"/messages?"+q.Encode(), nil)
			if res.IsError || noMark {
				return res, nil
			}
			page, err := decodeChatResult(res)
			if err != nil {
				return dispatcherErrorResult(key, "decode messages", err), nil
			}
			if seq := cli.ChatReadSeq(page.Messages, page.ReadSeq); seq > 0 {
				mark := d.chatRequest(ctx, key, user, http.MethodPost, path+"/read", map[string]int64{"seq": seq})
				return withChatMark(res, chatMarkStatus(mark)), nil
			}
			note := map[string]any{"marked": false, "reason": "empty", "read_seq": page.ReadSeq}
			if len(page.Messages) > 0 {
				first := page.Messages[0].Seq
				for _, msg := range page.Messages[1:] {
					if msg.Seq < first {
						first = msg.Seq
					}
				}
				note["reason"], note["first_unshown"], note["last_unshown"] = "gap", page.ReadSeq+1, first-1
			}
			return withChatMark(res, note), nil
		case "send":
			res := d.chatRequest(ctx, key, user, http.MethodPost, path+"/messages", in)
			if res.IsError {
				return res, nil
			}
			if in.ReplyToSeq != nil {
				mark := d.chatRequest(ctx, key, user, http.MethodPost, path+"/read", map[string]int64{"seq": *in.ReplyToSeq})
				return withChatMark(res, chatMarkStatus(mark)), nil
			}
			return res, nil
		case "delete":
			return d.chatRequest(ctx, key, user, http.MethodDelete, path+"/messages/"+strconv.FormatInt(seq, 10), nil), nil
		case "read":
			return d.chatRequest(ctx, key, user, http.MethodPost, path+"/read", map[string]int64{"seq": seq}), nil
		default:
			return invalid(fmt.Errorf("unknown chat action %q", verb))
		}
	}
}

// Follow-up marking must never replace a successful fetch or stored send.
func withChatMark(res *CallToolResult, mark map[string]any) *CallToolResult {
	response, ok := res.StructuredContent.(map[string]any)
	if !ok || response == nil {
		raw, _ := json.Marshal(map[string]any{"mark": mark})
		res.Content = append(res.Content, textResult(string(raw)).Content...)
		return res
	}
	response["mark"] = mark
	raw, _ := json.Marshal(response)
	return structuredResult(response, string(raw))
}

func chatMarkStatus(mark *CallToolResult) map[string]any {
	if mark.IsError {
		raw, _ := json.Marshal(mark.StructuredContent)
		var envelope ErrorEnvelope
		_ = json.Unmarshal(raw, &envelope)
		return map[string]any{"marked": false, "error": fmt.Sprintf("%s: %s", envelope.Error.Code, envelope.Error.Message)}
	}
	page, err := decodeChatResult(mark)
	if err != nil {
		return map[string]any{"marked": false, "error": err.Error()}
	}
	return map[string]any{"marked": true, "read_seq": page.ReadSeq}
}
