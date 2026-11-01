package prompts

type Prompt struct {
	Name         string   `anansi:"name,required=true" json:"name"`
	Version      string   `anansi:"version,required=true" json:"version"`
	Description  string   `anansi:"description,required=true" json:"description"`
	Category     string   `anansi:"category,required=true" json:"category"`
	Subcategory  string   `anansi:"subcategory" json:"subcategory,omitempty"`
	Tags         []string `anansi:"tags" json:"tags,omitempty"`
	Input        string   `anansi:"input" json:"input,omitempty"`
	Output       string   `anansi:"output" json:"output,omitempty"`
	Perspective  string   `anansi:"perspective" json:"perspective,omitempty"`
	OptimizesFor []string `anansi:"optimizes_for" json:"optimizes_for,omitempty"`
	Excludes     []string `anansi:"excludes" json:"excludes,omitempty"`
	Instructions string   `anansi:"instructions,required=true" json:"instructions"`
}

