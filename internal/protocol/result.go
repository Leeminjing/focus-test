package protocol

type ExecutionResult struct {
	ProtocolVersion int    `json:"protocol_version"`
	Mode            string `json:"mode"`
	Binding
	Status          string   `json:"status"`
	Outcome         string   `json:"outcome"`
	ExecutionStatus string   `json:"execution_status"`
	ReportStatus    string   `json:"report_status"`
	CleanupStatus   string   `json:"cleanup_status"`
	SnapshotID      string   `json:"snapshot_id"`
	ExecutionKey    string   `json:"execution_key"`
	TestIDs         []string `json:"test_ids"`
	Completed       int      `json:"completed"`
	Failed          int      `json:"failed"`
	Skipped         int      `json:"skipped"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Workspace       string   `json:"workspace,omitempty"`
	Diagnostic      string   `json:"diagnostic,omitempty"`
	ResultDigest    string   `json:"result_digest,omitempty"`
}
