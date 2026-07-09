package main

import (
	"encoding/json"
	"fmt"
	goMime "mime"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// attachmentCmd is the root for `pad attachment ...` operations.
//
// Phase 1 ships upload + download — the endpoints TASK-871 and TASK-872
// added. List + delete subcommands are intentionally absent because the
// underlying endpoints don't exist yet (they ship with TASK-881 / a
// future GC task). Adding clients that hit 404s is worse than not
// shipping them — same logic that kept "url" out of the upload response
// until the GET handler landed.
func attachmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachment",
		Short: "Upload, download, view, and list item attachments",
		RunE:  unknownSubcommandRun,
		Long: `Upload, download, view, and list attachments (images, files) on items.

Examples:
  pad attachment list                                # all workspace attachments
  pad attachment list --item TASK-5                  # attachments on a specific item
  pad attachment list --category image --limit 20    # filter + paginate
  pad attachment show <attachment-id>                # metadata only (HEAD)
  pad attachment view <attachment-id>                # save to temp file, print path
  pad attachment view <attachment-id> -o ./pic.png   # save to a chosen path
  pad attachment upload TASK-5 ./screenshot.png      # upload + attach to item
  pad attachment download <attachment-id> ./pic.png  # download to explicit path

Attachments belong to a workspace and may optionally reference an item.
Pass "-" as the item argument to upload without associating with any item.

For agents: ALWAYS use these CLI commands to read attachments — never read
directly from ~/.pad/attachments/. The CLI goes through the authenticated
REST API, which works on local SQLite, Pad Cloud, and remote/Postgres
deployments and respects workspace ACLs.`,
	}

	cmd.AddCommand(
		attachmentUploadCmd(),
		attachmentDownloadCmd(),
		attachmentViewCmd(),
		attachmentShowCmd(),
		attachmentListCmd(),
	)
	return cmd
}

func attachmentUploadCmd() *cobra.Command {
	var filenameFlag string

	cmd := &cobra.Command{
		Use:   "upload <item-ref-or-dash> <path>",
		Short: "Upload a file as an item attachment",
		Long: `Upload a file. The first argument is the parent item (issue ref or slug).
Use "-" to upload without associating with any item.

Examples:
  pad attachment upload TASK-5 ./screenshot.png
  pad attachment upload - ./standalone.pdf
  pad attachment upload TASK-5 ./design.pdf --filename "Design v2.pdf"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			itemArg := args[0]
			path := args[1]

			// "-" means "no parent item".
			itemRef := ""
			if itemArg != "" && itemArg != "-" {
				// Resolve via GetItem so the user can pass either a ref
				// like TASK-5 or a slug — and we fail fast with a useful
				// error if the item doesn't exist.
				it, err := client.GetItem(ws, itemArg)
				if err != nil {
					return fmt.Errorf("resolve item %q: %w", itemArg, err)
				}
				itemRef = it.ID
			}

			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %s: %w", path, err)
			}
			defer f.Close()

			filename := filenameFlag
			if filename == "" {
				filename = filepath.Base(path)
			}

			result, err := client.UploadAttachment(ws, itemRef, filename, f)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(result)
			}

			fmt.Printf("Uploaded %s (%s, %d bytes)\n", result.ID, result.MIME, result.Size)
			fmt.Printf("URL: %s\n", result.URL)
			if result.Width != nil && result.Height != nil {
				fmt.Printf("Dimensions: %d × %d\n", *result.Width, *result.Height)
			}
			fmt.Printf("Render mode: %s (category: %s)\n", result.RenderMode, result.Category)
			return nil
		},
	}

	cmd.Flags().StringVar(&filenameFlag, "filename", "", "override the stored filename (defaults to basename of path)")
	return cmd
}

func attachmentDownloadCmd() *cobra.Command {
	var variantFlag string

	cmd := &cobra.Command{
		Use:   "download <attachment-id> <out-path>",
		Short: "Download an attachment by ID",
		Long: `Download the bytes of an attachment by its UUID. Pass "-" as the out
path to stream to stdout (useful for piping into image viewers etc.).

Examples:
  pad attachment download <id> ./screenshot.png
  pad attachment download <id> --variant thumb-sm ./thumb.png
  pad attachment download <id> -  | open -f -a Preview`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			id := args[0]
			outPath := args[1]

			// Stdout case: we can't roll back partial output, so any
			// failure mid-stream is just visible as a short payload.
			if outPath == "-" {
				mime, n, err := client.DownloadAttachment(ws, id, variantFlag, os.Stdout)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "wrote %d bytes (%s)\n", n, mime)
				return nil
			}

			// File case: write to a sibling temp file first and rename
			// on success. Shared helper keeps `download` and
			// `view -o <path>` on the same crash-safe code path.
			mime, n, err := downloadAttachmentToPath(client, ws, id, variantFlag, outPath)
			if err != nil {
				return err
			}
			fmt.Printf("Saved %d bytes (%s) to %s\n", n, mime, outPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&variantFlag, "variant", "", "request a derived variant (thumb-sm | thumb-md)")
	return cmd
}

// downloadAttachmentToPath streams the attachment into a sibling temp
// file and atomically renames it onto outPath on success. Shared by
// `attachment download` and `attachment view -o <path>` so both get
// the same crash-safe behavior (a bad ID, auth failure, or partial
// download never truncates an existing destination — Codex round 1
// P2 on the original download command).
func downloadAttachmentToPath(client *cli.Client, ws, id, variant, outPath string) (mime string, n int64, err error) {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".*.tmp")
	if err != nil {
		return "", 0, fmt.Errorf("create download temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	mime, n, err = client.DownloadAttachment(ws, id, variant, tmp)
	if err != nil {
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close temp: %w", err)
	}
	// os.Rename atomically replaces an existing destination on every
	// supported platform (POSIX rename(2); Windows MoveFileEx with
	// MOVEFILE_REPLACE_EXISTING since Go 1.5).
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", 0, fmt.Errorf("rename %s -> %s: %w", tmpPath, outPath, err)
	}
	committed = true
	return mime, n, nil
}

// parseAttachmentFilename pulls the filename parameter out of a
// Content-Disposition header. Returns "" when the header is absent
// or unparseable so the caller can fall back to a synthetic name.
// Always passes the result through filepath.Base to defuse a
// hostile "../etc/passwd"-style filename — defense in depth even
// though the server already sanitizes on upload.
func parseAttachmentFilename(disposition string) string {
	if disposition == "" {
		return ""
	}
	_, params, err := goMime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	name := params["filename"]
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}

// extensionForMIME returns a leading-dot extension for a MIME type,
// or "" if the MIME isn't on our known list. Used as a last-resort
// fallback when the Content-Disposition header is missing a filename
// — we'd rather give the agent `<id>.png` than `<id>` with no hint
// for downstream tooling.
//
// We can't use mime.ExtensionsByType here because its results depend
// on the host's /etc/mime.types and aren't deterministic across
// platforms (Linux/macOS/Windows all differ). The hardcoded map
// mirrors the canonical entries in internal/attachments/mime.go.
func extensionForMIME(mimeType string) string {
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "audio/aac":
		return ".aac"
	case "audio/mp4":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	}
	return ""
}

func attachmentViewCmd() *cobra.Command {
	var outFlag string
	var variantFlag string

	cmd := &cobra.Command{
		Use:   "view <attachment-id>",
		Short: "Fetch an attachment to a file and print its path",
		Long: `Fetch an attachment by UUID through the authenticated REST API
and save it to disk. Prints the absolute path of the saved file on
stdout — designed for agents to use as $(pad attachment view <id>).

With no -o flag, the file lands in a fresh OS temp directory. The
filename comes from the attachment's Content-Disposition header so
agents can hand the path to image-viewing tools without rewriting
the extension.

With -o <path>, the file is written to that path using the same
atomic temp-then-rename strategy as 'pad attachment download'.

This is the recommended way for AI agents (Claude Code, Cursor, etc.)
to view attachments referenced in item content as
![alt](pad-attachment:<uuid>). It works for every Pad install
(local SQLite, Pad Cloud, remote/Postgres) and respects workspace
ACLs. Reading directly from ~/.pad/attachments/ does NOT — never
do that.

Examples:
  pad attachment view <id>                         # tmp file, print path
  pad attachment view <id> -o ./screenshot.png     # save to chosen path
  pad attachment view <id> --variant thumb-md      # serve a derived variant
  pad attachment view <id> --format json           # {path,mime,size}`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			id := args[0]

			outPath := outFlag
			if outPath == "" {
				// HEAD first so we can name the temp file using the
				// real filename + extension. Cheap (no body) and
				// gives us the size for the JSON output too.
				meta, err := client.HeadAttachment(ws, id, variantFlag)
				if err != nil {
					return err
				}
				name := parseAttachmentFilename(meta.ContentDisposition)
				if name == "" {
					name = id + extensionForMIME(meta.MIME)
				}
				dir, err := os.MkdirTemp("", "pad-attachment-")
				if err != nil {
					return fmt.Errorf("create temp dir: %w", err)
				}
				outPath = filepath.Join(dir, name)
			}

			mime, n, err := downloadAttachmentToPath(client, ws, id, variantFlag, outPath)
			if err != nil {
				return err
			}

			abs, err := filepath.Abs(outPath)
			if err != nil {
				abs = outPath
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"path": abs,
					"mime": mime,
					"size": n,
				})
			}
			// Default: just the path on stdout. Anything chatty goes
			// to stderr so $(pad attachment view <id>) substitutes
			// cleanly into a shell pipeline.
			fmt.Println(abs)
			fmt.Fprintf(os.Stderr, "wrote %d bytes (%s)\n", n, mime)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outFlag, "output", "o", "", "save to this path instead of an OS temp file")
	cmd.Flags().StringVar(&variantFlag, "variant", "", "request a derived variant (thumb-sm | thumb-md)")
	return cmd
}

func attachmentShowCmd() *cobra.Command {
	var variantFlag string

	cmd := &cobra.Command{
		Use:   "show <attachment-id>",
		Short: "Show attachment metadata (size, MIME, filename) without downloading",
		Long: `Issue a HEAD request and print the attachment's MIME type,
size, filename, ETag, and Last-Modified — without transferring the
bytes. Useful to confirm an attachment exists, or to size a
download before committing to it.

Examples:
  pad attachment show <id>
  pad attachment show <id> --format json
  pad attachment show <id> --variant thumb-sm`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			id := args[0]

			meta, err := client.HeadAttachment(ws, id, variantFlag)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				out := map[string]any{
					"id":   meta.ID,
					"mime": meta.MIME,
					"size": meta.Size,
				}
				if name := parseAttachmentFilename(meta.ContentDisposition); name != "" {
					out["filename"] = name
				}
				if meta.ETag != "" {
					out["etag"] = meta.ETag
				}
				if meta.LastModified != "" {
					out["last_modified"] = meta.LastModified
				}
				return cli.PrintJSON(out)
			}

			fmt.Printf("%-15s %s\n", "ID:", meta.ID)
			fmt.Printf("%-15s %s\n", "MIME:", meta.MIME)
			fmt.Printf("%-15s %s\n", "Size:", humanSize(meta.Size))
			if name := parseAttachmentFilename(meta.ContentDisposition); name != "" {
				fmt.Printf("%-15s %s\n", "Filename:", name)
			}
			if meta.ETag != "" {
				fmt.Printf("%-15s %s\n", "ETag:", meta.ETag)
			}
			if meta.LastModified != "" {
				fmt.Printf("%-15s %s\n", "Last-Modified:", meta.LastModified)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&variantFlag, "variant", "", "request a derived variant (thumb-sm | thumb-md)")
	return cmd
}

func attachmentListCmd() *cobra.Command {
	var (
		itemFlag       string
		categoryFlag   string
		collectionFlag string
		attachedFlag   bool
		unattachedFlag bool
		sortFlag       string
		limitFlag      int
		offsetFlag     int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List attachments in the workspace",
		Long: `List attachments in the current workspace, with optional
filters. Returns the same fields the web UI uses (id, mime, size,
filename, parent item, collection, created_at).

Examples:
  pad attachment list                              # all originals
  pad attachment list --item TASK-5                # one item's attachments
  pad attachment list --category image --limit 20  # images only
  pad attachment list --unattached                 # orphan uploads
  pad attachment list --format json                # parseable output

The --item flag accepts an item ref (TASK-5) or slug; the CLI
resolves it to a UUID before calling the API.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if attachedFlag && unattachedFlag {
				return fmt.Errorf("--attached and --unattached are mutually exclusive")
			}
			if itemFlag != "" && unattachedFlag {
				return fmt.Errorf("--item and --unattached are mutually exclusive")
			}

			client, _ := getClient()
			ws := getWorkspace()

			params := cli.AttachmentListParams{
				Category:     categoryFlag,
				CollectionID: collectionFlag,
				Sort:         sortFlag,
				Limit:        limitFlag,
				Offset:       offsetFlag,
			}
			if itemFlag != "" {
				it, err := client.GetItem(ws, itemFlag)
				if err != nil {
					return fmt.Errorf("resolve item %q: %w", itemFlag, err)
				}
				params.ItemID = it.ID
			}
			if attachedFlag {
				params.Item = "attached"
			}
			if unattachedFlag {
				params.Item = "unattached"
			}

			resp, err := client.ListAttachments(ws, params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(resp)
			}

			if len(resp.Attachments) == 0 {
				fmt.Println("No attachments.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tMIME\tSIZE\tFILENAME\tITEM\tCREATED")
			for _, raw := range resp.Attachments {
				var row struct {
					ID             string  `json:"id"`
					MimeType       string  `json:"mime_type"`
					SizeBytes      int64   `json:"size_bytes"`
					Filename       string  `json:"filename"`
					ItemTitle      *string `json:"item_title"`
					CollectionSlug *string `json:"collection_slug"`
					ItemDeleted    bool    `json:"item_deleted"`
					CreatedAt      string  `json:"created_at"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					continue
				}
				item := "—"
				if row.ItemTitle != nil && *row.ItemTitle != "" {
					item = *row.ItemTitle
					if row.ItemDeleted {
						item += " (deleted)"
					}
				}
				short := row.ID
				if len(short) > 8 {
					short = short[:8]
				}
				created := row.CreatedAt
				if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
					created = t.Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					short, row.MimeType, humanSize(row.SizeBytes), row.Filename, item, created)
			}
			tw.Flush()
			fmt.Printf("\n%d of %d (limit %d, offset %d)\n", len(resp.Attachments), resp.Total, resp.Limit, resp.Offset)
			return nil
		},
	}

	cmd.Flags().StringVar(&itemFlag, "item", "", "filter to attachments on a specific item (ref or slug)")
	cmd.Flags().StringVar(&categoryFlag, "category", "", "filter by MIME category: image|video|audio|document|text|archive|other")
	cmd.Flags().StringVar(&collectionFlag, "collection", "", "filter by collection UUID")
	cmd.Flags().BoolVar(&attachedFlag, "attached", false, "only attachments associated with an item")
	cmd.Flags().BoolVar(&unattachedFlag, "unattached", false, "only orphan attachments (no parent item)")
	cmd.Flags().StringVar(&sortFlag, "sort", "", "sort: size|size_desc|filename|filename_desc|created_at|created_at_desc (default created_at_desc)")
	cmd.Flags().IntVar(&limitFlag, "limit", 0, "page size (1-200, default 50)")
	cmd.Flags().IntVar(&offsetFlag, "offset", 0, "page offset")
	return cmd
}

// humanSize formats a byte count in a compact human-readable form
// (1.2 MB, 340 KB). Used by attachment list / show; not exported because
// the formatting is opinionated for those tables specifically.
func humanSize(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
