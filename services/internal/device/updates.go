package device

import "context"

// Field order is the D-Bus contract, including RFC3339 timestamps.
type Release struct {
	Tag        string
	BundleURL  string
	Size       uint64
	SumsURL    string
	Notes      string
	Published  string
	Prerelease bool
	Ready      bool
	Problem    string
	Blocked    string
}
type UpdateStatus struct {
	Current        string
	State          string
	Progress       int32
	Error          string
	Checked        string
	Checking       bool
	CheckError     string
	Target         string
	LastResult     string
	Suspended      bool
	RetryRequired  bool
	PendingAuto    bool
	PendingChannel string
	LastWindow     string
	OperationID    string
}

func (c *Client) UpdateStatus(ctx context.Context) (UpdateStatus, error) {
	var status UpdateStatus
	err := c.Property(ctx, "Updates", "Status", &status)
	return status, err
}
func (c *Client) UpdatesConfigured(ctx context.Context) (bool, error) {
	var capabilities []string
	err := c.Property(ctx, "Manager", "Capabilities", &capabilities)
	for _, domain := range capabilities {
		if domain == "updates" {
			return true, err
		}
	}
	return false, err
}
func (c *Client) Releases(ctx context.Context, channel string) ([]Release, error) {
	call, err := c.Call(ctx, "Updates", "Releases", channel)
	var releases []Release
	if err == nil && call.Store(&releases) != nil {
		err = ErrUnavailable
	}
	return releases, err
}
func (c *Client) CheckUpdates(ctx context.Context) ([]Release, error) {
	call, err := c.Call(ctx, "Updates", "Check")
	var releases []Release
	if err == nil && call.Store(&releases) != nil {
		err = ErrUnavailable
	}
	return releases, err
}
func (c *Client) InstallUpdate(ctx context.Context, tag, channel string, automatic, retry bool) (string, error) {
	call, err := c.Call(ctx, "Updates", "Install", tag, channel, automatic, retry)
	var operation string
	if err == nil && (call.Store(&operation) != nil || operation == "") {
		err = ErrUnavailable
	}
	return operation, err
}
