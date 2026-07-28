package sandboxlimits

const (
	// ToolExecRequestBodyBytes caps encoded JSON tool requests at every hop.
	// Base64 file writes get roughly 3/4 of this as guaranteed raw payload.
	ToolExecRequestBodyBytes int64 = 8 << 20

	// ControlClientResponseBytes is the maximum marshaled guest control response.
	ControlClientResponseBytes int64 = 16 << 20

	// ToolReadRawBytes is a fast raw-file read ceiling. The guest also checks the
	// marshaled response size because JSON/base64 encoding can inflate content.
	ToolReadRawBytes int64 = 11 << 20

	// FileTransferMaxBytes caps raw workspace file transfer streams. Unlike the
	// tool-operation caps above, file transfers move octet streams outside JSON.
	FileTransferMaxBytes int64 = 1 << 30
)
