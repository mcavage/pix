package uat

type ToolDescriptor struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	ReadOnly    bool                   `json:"readOnly,omitempty"`
	Destructive bool                   `json:"destructive,omitempty"`
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

func GetToolDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		{
			Name:        "submit",
			Description: "Submit UAT results",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"results": map[string]interface{}{"type": "string"},
				},
				"required": []string{"results"},
			},
			ReadOnly:    false,
			Destructive: false,
		},
		{
			Name:        "status",
			Description: "Get UAT status",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
			ReadOnly:    true,
			Destructive: false,
		},
		{
			Name:        "artifact",
			Description: "Get UAT artifact",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string"},
				},
				"required": []string{"id"},
			},
			ReadOnly:    true,
			Destructive: false,
		},
		{
			Name:        "browser_action",
			Description: "Perform browser action",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{"type": "string"},
				},
				"required": []string{"action"},
			},
			ReadOnly:    false,
			Destructive: false,
		},
		{
			Name:        "abort",
			Description: "Abort UAT",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
			ReadOnly:    false,
			Destructive: true,
		},
	}
}
