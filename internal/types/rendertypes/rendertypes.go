package rendertypes

type Repo struct {
	Name      string
	Path      string
	Languages []string
	Identity  map[string]string
}

type Input struct {
	Root         string
	Repos        []Repo
	Dependencies map[string]string
	Dev          map[string]string
}

type Command struct {
	Dir      string
	Command  string
	Args     []string
	Env      map[string]string
	Optional bool
}

type Output struct {
	Files          []File
	DependencyLock []Command
	Lockfiles      []string
}

type File struct {
	Path       string
	Content    string
	Gitignore  string
	AlsoIgnore []string
	Merged     bool
}

func (r Repo) Speaks(language string) bool {
	for _, l := range r.Languages {
		if l == language {
			return true
		}
	}

	return false
}
