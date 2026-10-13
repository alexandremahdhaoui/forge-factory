package gomod

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

var ErrRefused = errors.New("refusing to edit a go.mod")

func Edit(content, module, goVersion string, versions map[string]string) (string, error) {
	set := map[string]string{"module": module, "go": goVersion}
	seen := map[string]bool{}

	edited, err := eachStatement(content, func(verb string, args []string, line string) string {
		if value, ok := set[verb]; ok {
			seen[verb] = true

			if value == "" {
				return line
			}

			return replaceAfter(line, verb, args[0], value)
		}

		if verb != "require" {
			return line
		}

		if declared, ok := versions[args[0]]; ok {
			return replaceAfter(line, args[0], args[1], declared)
		}

		return line
	})
	if err != nil {
		return "", err
	}

	for _, verb := range []string{"module", "go"} {
		if !seen[verb] {
			return "", fmt.Errorf("%w: it has no %s line", ErrRefused, verb)
		}
	}

	return edited, nil
}

func GoVersion(content string) (string, error) {
	version := ""

	_, err := eachStatement(content, func(verb string, args []string, line string) string {
		if verb == "go" {
			version = args[0]
		}

		return line
	})
	if err != nil {
		return "", err
	}

	if version == "" {
		return "", fmt.Errorf("%w: it has no go line", ErrRefused)
	}

	return version, nil
}

func Requires(content string) (map[string]bool, error) {
	out := map[string]bool{}

	_, err := eachStatement(content, func(verb string, args []string, line string) string {
		if verb == "require" {
			out[args[0]] = true
		}

		return line
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

var arity = map[string]int{"module": 1, "go": 1, "require": 2}

func eachStatement(content string, visit func(verb string, args []string, line string) string) (string, error) {
	lines := strings.Split(content, "\n")
	block := ""

	for i, line := range lines {
		fields := tokens(line)
		verb, args := block, fields

		switch {
		case len(fields) == 0:
			continue
		case block != "" && len(fields) == 1 && fields[0] == ")":
			block = ""

			continue
		case block == "" && len(fields) == 2 && fields[1] == "(":
			if fields[0] == "module" {
				return "", fmt.Errorf("%w: line %d opens a factored module block: %s", ErrRefused, i+1, strings.TrimSpace(line))
			}

			block = fields[0]

			continue
		case block == "":
			verb, args = fields[0], fields[1:]
		}

		if want, ok := arity[verb]; ok && !readable(args, want) {
			return "", fmt.Errorf("%w: line %d cannot be read: %s", ErrRefused, i+1, strings.TrimSpace(line))
		}

		lines[i] = visit(verb, args, line)
	}

	return strings.Join(lines, "\n"), nil
}

func tokens(line string) []string {
	code, _, _ := strings.Cut(line, "//")
	fields := strings.Fields(strings.NewReplacer("(", " ( ", ")", " ) ").Replace(code))

	for i, field := range fields {
		if unquoted, err := strconv.Unquote(field); err == nil {
			fields[i] = unquoted
		}
	}

	return fields
}

func readable(args []string, want int) bool {
	return len(args) == want && !slices.Contains(args, "(") && !slices.Contains(args, ")")
}

func replaceAfter(line, anchor, old, value string) string {
	start := strings.Index(line, anchor) + len(anchor)
	at := start + strings.Index(line[start:], old)

	return line[:at] + value + line[at+len(old):]
}
