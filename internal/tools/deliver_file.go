package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// DeliverFileTool surfaces an EXISTING workspace file to the user as a chat
// download link. write_file(deliver=true) only covers text the LLM writes
// inline; binary deliverables — .xlsx / .docx / .pdf / images / archives —
// are produced by exec'd code (python+openpyxl, pandas, libreoffice, …) and
// then have no delivery path, so the user never gets a link. This tool closes
// that gap by reusing the exact write_file delivery machinery: mirror the file
// into the media store (S3 on stage/prod) and attach it as Media, so the chat
// renders a signed /v1/files download URL that survives the workspace cleanup
// cron.
type DeliverFileTool struct {
	workspace       string
	restrict        bool
	allowedPrefixes []string
	deniedPrefixes  []string
	mediaUpload     MediaUploadFunc
}

// NewDeliverFileTool creates a DeliverFileTool bound to a workspace root.
func NewDeliverFileTool(workspace string, restrict bool) *DeliverFileTool {
	return &DeliverFileTool{workspace: workspace, restrict: restrict}
}

// SetMediaUploadFunc enables durable copy of delivered files to the media store
// (wired alongside WriteFileTool's hook). Without it, delivery falls back to the
// local workspace path (works immediately, pruned after the 7d cleanup cron).
func (t *DeliverFileTool) SetMediaUploadFunc(fn MediaUploadFunc) { t.mediaUpload = fn }

// AllowPaths / DenyPaths mirror WriteFileTool so deliver_file honours the same
// path boundaries as write/read.
func (t *DeliverFileTool) AllowPaths(prefixes ...string) {
	t.allowedPrefixes = append(t.allowedPrefixes, prefixes...)
}
func (t *DeliverFileTool) DenyPaths(prefixes ...string) {
	t.deniedPrefixes = append(t.deniedPrefixes, prefixes...)
}

func (t *DeliverFileTool) Name() string { return "deliver_file" }

func (t *DeliverFileTool) Description() string {
	return "Send an existing file from the workspace to the user as a downloadable attachment with a link. " +
		"Use this for files you generated with code/exec — spreadsheets (.xlsx, .csv), documents (.docx, .pdf), " +
		"images, archives (.zip), etc. — so the user can download them. The file must already exist on disk " +
		"(create it first via exec/write_file, then deliver it). Do not also send it via the message tool."
}

func (t *DeliverFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the existing file to deliver (relative to the workspace, or absolute).",
			},
		},
		"required": []string{"path"},
	}
}

func (t *DeliverFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, _ := args["path"].(string)
	if path == "" {
		return ErrorResult("path is required")
	}

	workspace := ToolWorkspaceFromCtx(ctx)
	if workspace == "" {
		workspace = t.workspace
	}
	allowed := allowedWithTeamWorkspace(ctx, t.allowedPrefixes)
	resolved, err := resolvePathWithAllowed(path, workspace, effectiveRestrict(ctx, t.restrict), allowed)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if err := checkDeniedPath(resolved, t.workspace, t.deniedPrefixes); err != nil {
		return ErrorResult(err.Error())
	}

	fi, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult(fmt.Sprintf("file does not exist: %s — create it first (e.g. via exec), then deliver_file it", path))
		}
		return ErrorResult(fmt.Sprintf("cannot access file: %v", err))
	}
	if fi.IsDir() {
		return ErrorResult(fmt.Sprintf("%s is a directory, not a file", path))
	}

	// Mirror to the durable media store when wired, so the attachment survives
	// the workspace cleanup cron; otherwise deliver the local path directly.
	deliveredPath := resolved
	if t.mediaUpload != nil {
		if cachePath := uploadDeliveredToMediaStore(ctx, t.mediaUpload, resolved); cachePath != "" {
			deliveredPath = cachePath
		}
	}

	result := SilentResult(fmt.Sprintf(
		"File delivered to the user: %s (%d bytes). A download link is attached to the chat — do NOT send it again via the message tool.",
		filepath.Base(resolved), fi.Size(),
	))
	result.Media = []bus.MediaFile{{Path: deliveredPath, Filename: filepath.Base(resolved)}}
	// Track delivered path so the message tool's self-send guard detects dupes.
	if dm := DeliveredMediaFromCtx(ctx); dm != nil {
		dm.Mark(deliveredPath)
	}
	return result
}
