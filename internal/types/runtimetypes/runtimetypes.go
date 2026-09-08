package runtimetypes

type Input struct {
	Runtime string
	Version string
	OS      string
	Arch    string
	Params  map[string]string
	Targets []string
}

type Pick struct {
	From string
	At   string
}

type Artifact struct {
	URL    string
	SHA256 string
	Unpack string
	Strip  int
	Picks  []Pick
}

type Prerequisite struct {
	Name   string
	Reason string
	Verify string
}

type Description struct {
	Runtime       string
	Version       string
	Artifacts     []Artifact
	Bins          []string
	Env           map[string]string
	Prerequisites []Prerequisite
	Provides      []string
}
