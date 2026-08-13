package uat

type ToolDescriptor struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Parameters  map[string]string `json:"parameters"`
}

type Request struct {
	ToolName string                 `json:"toolName"`
	Args     map[string]interface{} `json:"args"`
}

type Result struct {
	Status string                 `json:"status"`
	Output map[string]interface{} `json:"output"`
	Error  string                 `json:"error,omitempty"`
}
