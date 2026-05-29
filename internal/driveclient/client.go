package driveclient

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// FolderMime is the Drive MIME type for folders.
const FolderMime = "application/vnd.google-apps.folder"

// Client wraps a *drive.Service with retry/backoff applied to every call.
type Client struct {
	svc      *drive.Service
	lg       *slog.Logger
	attempts int
}

// New wraps svc; attempts is the maximum number of tries per API call.
func New(svc *drive.Service, lg *slog.Logger, attempts int) *Client {
	if attempts < 1 {
		attempts = 1
	}
	return &Client{svc: svc, lg: lg, attempts: attempts}
}

// EscapeQuery escapes a value for use inside a Drive query string literal.
func EscapeQuery(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\r", " ", "\n", " ")
	return r.Replace(s)
}

// List runs a files.list query and returns matching files. fields controls the
// partial response (must include nextPageToken when paginating via ListAll).
func (c *Client) List(ctx context.Context, query, fields, pageToken string) (*drive.FileList, error) {
	return Do(ctx, c.lg, "files.list", c.attempts, func() (*drive.FileList, error) {
		call := c.svc.Files.List().
			Q(query).
			Spaces("drive").
			Corpora("user").
			PageSize(1000).
			Fields(googleapi.Field(fields)).
			IncludeItemsFromAllDrives(true).
			SupportsAllDrives(true).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		return call.Do()
	})
}

// ListAll pages through a query, invoking yield for every file.
func (c *Client) ListAll(ctx context.Context, query, fields string, yield func(*drive.File) error) error {
	token := ""
	for {
		res, err := c.List(ctx, query, fields, token)
		if err != nil {
			return err
		}
		for _, f := range res.Files {
			if err := yield(f); err != nil {
				return err
			}
		}
		token = res.NextPageToken
		if token == "" {
			return nil
		}
	}
}

// Get fetches a single file's metadata.
func (c *Client) Get(ctx context.Context, fileID, fields string) (*drive.File, error) {
	return Do(ctx, c.lg, "files.get", c.attempts, func() (*drive.File, error) {
		return c.svc.Files.Get(fileID).Fields(googleapi.Field(fields)).SupportsAllDrives(true).Context(ctx).Do()
	})
}

// Create creates a file/folder and returns the requested fields.
func (c *Client) Create(ctx context.Context, f *drive.File, fields string) (*drive.File, error) {
	return Do(ctx, c.lg, "files.create", c.attempts, func() (*drive.File, error) {
		return c.svc.Files.Create(f).Fields(googleapi.Field(fields)).SupportsAllDrives(true).Context(ctx).Do()
	})
}

// Copy copies srcID into a new file described by f.
func (c *Client) Copy(ctx context.Context, srcID string, f *drive.File, fields string) (*drive.File, error) {
	return Do(ctx, c.lg, "files.copy", c.attempts, func() (*drive.File, error) {
		return c.svc.Files.Copy(srcID, f).Fields(googleapi.Field(fields)).SupportsAllDrives(true).Context(ctx).Do()
	})
}

// Update patches a file with the given partial body. forceSend lists JSON field
// names that must be sent even when zero-valued (e.g. "Trashed").
func (c *Client) Update(ctx context.Context, fileID string, f *drive.File, fields string, forceSend ...string) (*drive.File, error) {
	return Do(ctx, c.lg, "files.update", c.attempts, func() (*drive.File, error) {
		f.ForceSendFields = forceSend
		return c.svc.Files.Update(fileID, f).Fields(googleapi.Field(fields)).SupportsAllDrives(true).Context(ctx).Do()
	})
}

// Share grants a user permission on a file.
func (c *Client) Share(ctx context.Context, fileID, email, role string, notify bool) (*drive.Permission, error) {
	return Do(ctx, c.lg, "permissions.create", c.attempts, func() (*drive.Permission, error) {
		p := &drive.Permission{Type: "user", Role: role, EmailAddress: email}
		return c.svc.Permissions.Create(fileID, p).
			SendNotificationEmail(notify).
			SupportsAllDrives(true).
			Fields("id").
			Context(ctx).
			Do()
	})
}
