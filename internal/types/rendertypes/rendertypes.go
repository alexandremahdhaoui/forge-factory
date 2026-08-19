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
}

// File is one file a renderer wants written.
type File struct {
	Path      string
	Content   string
	Gitignore string
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
