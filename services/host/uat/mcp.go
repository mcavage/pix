package uat

type ToolDescriptor struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
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
					"results":          map[string]interface{}{"type": "string"},
					"uat_capabilities": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				},
				"required": []string{"results", "uat_capabilities"},
			},
		},
		{
			Name:        "status",
			Description: "Get UAT status",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
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
		},
		{
			Name:        "abort",
			Description: "Abort UAT",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
		},
	}
}
