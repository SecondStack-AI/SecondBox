package toolexecutor

// ContractVersion changes whenever the host/guest request or response shape is
// no longer backwards compatible.
const ContractVersion = 1

type Operation string

const (
	OpExec           Operation = "exec"
	OpReadFile       Operation = "readFile"
	OpReadFileBuffer Operation = "readFileBuffer"
	OpWriteFile      Operation = "writeFile"
	OpStat           Operation = "stat"
	OpReaddir        Operation = "readdir"
	OpExists         Operation = "exists"
	OpMkdir          Operation = "mkdir"
	OpRm             Operation = "rm"
)

var operations = []Operation{
	OpExec,
	OpReadFile,
	OpReadFileBuffer,
	OpWriteFile,
	OpStat,
	OpReaddir,
	OpExists,
	OpMkdir,
	OpRm,
}

func Operations() []Operation {
	return append([]Operation(nil), operations...)
}

type Request struct {
	Operation     Operation         `json:"operation"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	TimeoutMillis int64             `json:"timeoutMs,omitempty"`
	Path          string            `json:"path,omitempty"`
	Content       string            `json:"content,omitempty"`
	ContentBase64 string            `json:"contentBase64,omitempty"`
	Encoding      string            `json:"encoding,omitempty"`
	Recursive     bool              `json:"recursive,omitempty"`
	Force         bool              `json:"force,omitempty"`
}

type Response struct {
	Stdout        string         `json:"stdout,omitempty"`
	Stderr        string         `json:"stderr,omitempty"`
	ExitCode      int            `json:"exitCode,omitempty"`
	TimedOut      bool           `json:"timedOut,omitempty"`
	Content       string         `json:"content,omitempty"`
	ContentBase64 string         `json:"contentBase64,omitempty"`
	Stat          map[string]any `json:"stat,omitempty"`
	Entries       []DirEntry     `json:"entries,omitempty"`
	Exists        *bool          `json:"exists,omitempty"`
	Error         string         `json:"error,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size,omitempty"`
	Mtime string `json:"mtime,omitempty"`
}
