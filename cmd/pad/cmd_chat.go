package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

func chatListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Example: "  pad chat list", Short: "List your global chat conversations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			raw, err := client.ListChatConversations()
			if err != nil {
				return err
			}
			return printChatResponse(cmd, "list", raw)
		}}
}

func chatInboxCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{Use: "inbox", Example: "  pad chat inbox --agent rook", Short: "Show unread chat counts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			raw, err := client.ChatUnread(agent)
			if err != nil {
				return err
			}
			return printChatResponse(cmd, "inbox", raw)
		}}
	cmd.Flags().StringVar(&agent, "agent", "", "Filter by agent key")
	return cmd
}

func chatShowCmd() *cobra.Command {
	var since int64
	var limit int
	var noMark bool
	cmd := &cobra.Command{Use: "show <target>", Example: "  pad chat show C12 --since 4", Short: "Show chat messages and mark read when no unread messages are skipped", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := cli.ParseChatTarget(args[0]); err != nil {
				return err
			}
			if limit < 1 || limit > 200 {
				return fmt.Errorf("--limit must be between 1 and 200")
			}
			client, _ := getClient()
			n, err := client.ResolveChatTarget(args[0], false)
			if err != nil {
				return err
			}
			var after *int64
			if cmd.Flags().Changed("since") {
				after = &since
			}
			raw, err := client.ChatMessages(n, after, limit)
			if err != nil {
				return err
			}
			if noMark {
				return printChatResponse(cmd, "show", raw)
			}
			if formatFlag != "json" {
				if err := printChatResponse(cmd, "show", raw); err != nil {
					return err
				}
			}
			var page struct {
				ReadSeq  int64 `json:"read_seq"`
				Messages []struct {
					Seq int64 `json:"seq"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return err
			}
			mark := map[string]any{"marked": false, "reason": "empty", "read_seq": page.ReadSeq}
			if seq := cli.ChatReadSeq(page.Messages, page.ReadSeq); seq > 0 {
				mark, err = chatReadMark(client, n, seq)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: messages displayed, but marking read failed: %v\n", err)
				}
			} else if len(page.Messages) > 0 {
				first := page.Messages[0].Seq
				for _, msg := range page.Messages[1:] {
					if msg.Seq < first {
						first = msg.Seq
					}
				}
				mark["reason"], mark["first_unshown"], mark["last_unshown"] = "gap", page.ReadSeq+1, first-1
				fmt.Fprintf(cmd.ErrOrStderr(), "Not marked read: messages #%d-#%d were not shown; run pad chat show C%d --since %d\n", page.ReadSeq+1, first-1, n, page.ReadSeq)
			}
			if formatFlag == "json" {
				return printChatJSONWithMark(cmd, raw, mark)
			}
			return nil
		}}
	cmd.Flags().Int64Var(&since, "since", 0, "Show messages after this sequence")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum messages to return")
	cmd.Flags().BoolVar(&noMark, "no-mark", false, "Do not mark returned messages read")
	return cmd
}

func chatSendCmd() *cobra.Command {
	var replyTo int64
	var opKey, contextWorkspace string
	cmd := &cobra.Command{Use: "send <target> <body>", Example: "  pad chat send @rook \"Ready for review\"", Short: "Send a chat message (- reads stdin)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := cli.ParseChatTarget(args[0]); err != nil {
				return err
			}
			in := cli.ChatSendInput{Body: args[1], ContextWorkspace: contextWorkspace}
			if in.Body == "-" {
				body, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				in.Body = string(body)
			}
			if cmd.Flags().Changed("reply-to") {
				if replyTo < 1 {
					return fmt.Errorf("--reply-to must be at least 1")
				}
				in.ReplyToSeq = &replyTo
			}
			if cmd.Flags().Changed("op-key") {
				in.OpKey = &opKey
			}
			if !cmd.Flags().Changed("context-workspace") {
				link, err := cli.LoadPadToml()
				if err != nil {
					return err
				}
				if link != nil {
					in.ContextWorkspace = link.Workspace
				}
			}
			client, _ := getClient()
			n, err := client.ResolveChatTarget(args[0], true)
			if err != nil {
				return err
			}
			raw, err := client.SendChatMessage(n, in)
			if err != nil {
				return err
			}
			if formatFlag != "json" {
				if err := printChatResponse(cmd, "send", raw); err != nil {
					return err
				}
			}
			if in.ReplyToSeq != nil {
				mark, err := chatReadMark(client, n, *in.ReplyToSeq)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: message sent, but marking reply read failed: %v\n", err)
				}
				if formatFlag == "json" {
					return printChatJSONWithMark(cmd, raw, mark)
				}
			}
			if formatFlag == "json" {
				return printChatResponse(cmd, "send", raw)
			}
			return nil
		}}
	cmd.Flags().Int64Var(&replyTo, "reply-to", 0, "Reply to this message sequence (marks read through this sequence)")
	cmd.Flags().StringVar(&opKey, "op-key", "", "Caller-owned idempotency key")
	cmd.Flags().StringVar(&contextWorkspace, "context-workspace", "", "Workspace for resolving message references (defaults to cwd link)")
	return cmd
}

func chatDeleteCmd() *cobra.Command {
	return &cobra.Command{Use: "delete <target> <seq>", Example: "  pad chat delete C12 4", Short: "Delete one of your chat messages", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := cli.ParseChatTarget(args[0]); err != nil {
				return err
			}
			seq, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || seq < 1 {
				return fmt.Errorf("seq must be a positive integer")
			}
			client, _ := getClient()
			n, err := client.ResolveChatTarget(args[0], false)
			if err != nil {
				return err
			}
			raw, err := client.DeleteChatMessage(n, seq)
			if err != nil {
				return err
			}
			return printChatResponse(cmd, "delete", raw)
		}}
}

func chatReadCmd() *cobra.Command {
	var to int64
	cmd := &cobra.Command{Use: "read <target>", Example: "  pad chat read C12 --to 4", Short: "Mark chat read through a message sequence", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := cli.ParseChatTarget(args[0]); err != nil {
				return err
			}
			if to < 0 {
				return fmt.Errorf("--to must be nonnegative")
			}
			client, _ := getClient()
			n, err := client.ResolveChatTarget(args[0], false)
			if err != nil {
				return err
			}
			raw, err := client.ReadChat(n, to)
			if err != nil {
				return err
			}
			return printChatResponse(cmd, "read", raw)
		}}
	cmd.Flags().Int64Var(&to, "to", 0, "Mark read through this sequence")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func chatReadMark(client *cli.Client, number int, seq int64) (map[string]any, error) {
	raw, err := client.ReadChat(number, seq)
	var result struct {
		ReadSeq int64 `json:"read_seq"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		return map[string]any{"marked": false, "error": err.Error()}, err
	}
	return map[string]any{"marked": true, "read_seq": result.ReadSeq}, nil
}

func printChatJSONWithMark(cmd *cobra.Command, raw json.RawMessage, mark map[string]any) error {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("chat response must be a JSON object")
	}
	note, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	response["mark"] = note
	return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
}

func printChatResponse(cmd *cobra.Command, action string, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	if formatFlag == "json" {
		_, err := fmt.Fprintln(out, string(raw))
		return err
	}
	var result struct {
		Conversations []struct {
			Number      int    `json:"number"`
			AgentKey    string `json:"agent_key"`
			Unread      int    `json:"unread"`
			LastMessage *struct {
				Excerpt string `json:"excerpt"`
				Deleted bool   `json:"deleted"`
			} `json:"last_message"`
		} `json:"conversations"`
		Total    int                          `json:"total"`
		Messages []models.ConversationMessage `json:"messages"`
		Message  models.ConversationMessage   `json:"message"`
		ReadSeq  int64                        `json:"read_seq"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	switch action {
	case "list", "inbox":
		for _, c := range result.Conversations {
			if action == "inbox" && c.Unread == 0 {
				continue
			}
			excerpt := ""
			if c.LastMessage != nil {
				excerpt = c.LastMessage.Excerpt
				if c.LastMessage.Deleted {
					excerpt = "Message deleted"
				}
			}
			if _, err := fmt.Fprintf(out, "C%d  @%s  unread: %d  %s\n", c.Number, c.AgentKey, c.Unread, excerpt); err != nil {
				return err
			}
		}
		if action == "inbox" {
			_, err := fmt.Fprintf(out, "Total unread: %d\n", result.Total)
			return err
		}
	case "show":
		for _, m := range result.Messages {
			body := m.Body
			if m.Deleted {
				body = "Message deleted"
			}
			author := m.AuthorAgentKey
			if author == "" {
				author = "human"
			} else {
				author = "@" + author
			}
			if _, err := fmt.Fprintf(out, "#%d %s: %s\n", m.Seq, author, body); err != nil {
				return err
			}
			if q := m.ReplyTo; q != nil {
				excerpt := q.Excerpt
				if q.Deleted {
					excerpt = "Message deleted"
				}
				if _, err := fmt.Fprintf(out, "  > #%d: %s\n", q.Seq, excerpt); err != nil {
					return err
				}
			}
		}
	case "send":
		_, err := fmt.Fprintf(out, "Sent #%d\n", result.Message.Seq)
		return err
	case "delete":
		_, err := fmt.Fprintf(out, "Deleted #%d\n", result.Message.Seq)
		return err
	case "read":
		_, err := fmt.Fprintf(out, "Read through #%d\n", result.ReadSeq)
		return err
	}
	return nil
}
