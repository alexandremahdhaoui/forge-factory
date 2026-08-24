package docstypes

type File struct {
	Path    string
	Content string
}

type Concept struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Detail  string   `json:"detail,omitempty"`
	SeeAlso []string `json:"seeAlso,omitempty"`
}

type Verb struct {
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary"`
}

type Concepts struct {
	Tool     string    `json:"tool"`
	Summary  string    `json:"summary"`
	Concepts []Concept `json:"concepts"`
	Verbs    []Verb    `json:"verbs"`
}

type Decision struct {
	Title    string `json:"title"`
	Date     string `json:"date"`
	Decision string `json:"decision"`
	Because  string `json:"because"`
}

type Decisions struct {
	Decisions []Decision `json:"decisions"`
}

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Input       string `json:"input,omitempty"`
	Output      string `json:"output,omitempty"`
}

type Engine struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Generate    struct {
		PackageName string `json:"packageName"`
		DocsBaseURL string `json:"docsBaseURL"`
	} `json:"generate"`
	Surface struct {
		Tools []Tool `json:"tools"`
	} `json:"surface"`
	OpenAPI struct {
		SpecPath string `json:"specPath"`
	} `json:"openapi"`
}
