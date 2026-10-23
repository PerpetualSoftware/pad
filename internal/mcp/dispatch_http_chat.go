package mcp

import (
	"context"
	"encoding/json"
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
	n, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	if err != nil || n < min {
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
			}{{"since", "after_seq", 0}, {"limit", "limit", 0}} {
				if _, ok := input[field.name]; ok {
					n, err := chatSequence(input, field.name, field.min)
					if err != nil {
						return invalid(err)
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
			if maxSeq := cli.ChatMaxReturnedSeq(page.Messages); maxSeq > 0 {
				mark := d.chatRequest(ctx, key, user, http.MethodPost, path+"/read", map[string]int64{"seq": maxSeq})
				if mark.IsError {
					return mark, nil
				}
			}
			return res, nil
		case "send":
			res := d.chatRequest(ctx, key, user, http.MethodPost, path+"/messages", in)
			if res.IsError {
				return res, nil
			}
			if in.ReplyToSeq != nil {
				mark := d.chatRequest(ctx, key, user, http.MethodPost, path+"/read", map[string]int64{"seq": *in.ReplyToSeq})
				if mark.IsError {
					return mark, nil
				}
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
