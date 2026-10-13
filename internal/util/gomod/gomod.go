package gomod

import "strings"

func SetVersions(content string, versions map[string]string) string {
	return eachRequire(content, func(module, version string) string {
		if declared, ok := versions[module]; ok {
			return declared
		}

		return version
	})
}

func Requires(content string) map[string]bool {
	out := map[string]bool{}

	eachRequire(content, func(module, version string) string {
		out[module] = true

		return version
	})

	return out
}

func eachRequire(content string, visit func(module, version string) string) string {
	lines := strings.Split(content, "\n")
	inBlock := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "//"):
		case inBlock && trimmed == ")":
			inBlock = false
		case inBlock:
			lines[i] = rewriteRequire(line, strings.Fields(trimmed), visit)
		case trimmed == "require (":
			inBlock = true
		case strings.HasPrefix(trimmed, "require "):
			lines[i] = rewriteRequire(line, strings.Fields(trimmed)[1:], visit)
		}
	}

	return strings.Join(lines, "\n")
}

func rewriteRequire(line string, fields []string, visit func(module, version string) string) string {
	if len(fields) < 2 {
		return line
	}

	module, version := fields[0], fields[1]
	end := strings.Index(line, module) + len(module)

	return line[:end] + strings.Replace(line[end:], version, visit(module, version), 1)
}
