package rendertypes

// Repo is one member of the workspace as a renderer sees it.
type Repo struct {
	Name      string
	Path      string
	Languages []string
	Identity  map[string]string
}

// Input is everything a renderer needs to decide what to write.
type Input struct {
	Root         string
	Repos        []Repo
	Dependencies map[string]string
	Dev          map[string]string
}

// Command is what to run after the files land. A generated go.mod names only
// the direct requires, so a tidy is what puts the indirect ones back.
type Command struct {
	Dir      string
	Command  string
	Args     []string
	Env      map[string]string
	Optional bool
}

// Output is everything a renderer decided.
type Output struct {
	Files  []File
	Settle []Command
}

// File is one file a renderer wants written.
type File struct {
	Path       string
	Content    string
	Gitignore  string
	AlsoIgnore []string
}

// Speaks reports whether a repo is written in a language.
func (r Repo) Speaks(language string) bool {
	for _, l := range r.Languages {
		if l == language {
			return true
		}
	}

	return false
}
