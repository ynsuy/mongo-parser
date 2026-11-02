// Package classify names composite symptom patterns built from the labels package label already
// computed: a classifier is a fixed vocabulary entry — "io_saturation", "cpu_saturation" — that
// requires several labels to each be at or past a named band of their own rule, on the same node,
// at once.
//
// A classifier measures nothing of its own; it only names a combination of measurements package
// label already made. It still does not diagnose: several labels being high together is a symptom
// pattern worth a fixed name, not a stated cause. Why it happened is still for whoever reads the
// labels beside the logs.
package classify

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"sort"

	"github.com/ynsuy/mongo-parser/internal/label"
	"gopkg.in/yaml.v3"
)

//go:embed classifiers/*.yaml
var classifierFiles embed.FS

// Pack is one classifier file: the classifiers for one subject.
type Pack struct {
	Pack        string       `yaml:"pack"`
	Description string       `yaml:"description"`
	Classifiers []Classifier `yaml:"classifiers"`
}

// Classifier is one named composite symptom pattern.
type Classifier struct {
	Name  string `json:"name" yaml:"name"`
	Title string `json:"title" yaml:"title"`
	// Doc says what the combination means, and why these labels together are worth a fixed name.
	Doc string `json:"doc" yaml:"doc"`
	// When is every condition that must hold, on the same node, for the classifier to match. Two or
	// more: one condition alone is just that label's own band, nothing a classifier need say twice.
	When []Condition `json:"when" yaml:"when"`
}

// Condition is one label required to be at or past a named band of its own rule.
type Condition struct {
	Label   string `json:"label" yaml:"label"`
	AtLeast string `json:"at_least" yaml:"at_least"`
}

// Classifiers is every classifier, loaded once.
var Classifiers = mustLoad()

func mustLoad() []Classifier {
	classifiers, err := load()
	if err != nil {
		panic(err)
	}
	return classifiers
}

func load() ([]Classifier, error) {
	entries, err := classifierFiles.ReadDir("classifiers")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]Classifier, 0, 16)
	seen := map[string]bool{}
	for _, name := range names {
		raw, err := classifierFiles.ReadFile(path.Join("classifiers", name))
		if err != nil {
			return nil, err
		}
		var pack Pack
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		// A misspelt field is a classifier that silently does nothing, so an unknown one is an error.
		decoder.KnownFields(true)
		if err := decoder.Decode(&pack); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, c := range pack.Classifiers {
			if err := c.validate(); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, c.Name, err)
			}
			if seen[c.Name] {
				return nil, fmt.Errorf("%s: %s is defined twice", name, c.Name)
			}
			seen[c.Name] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func (c Classifier) validate() error {
	switch {
	case c.Name == "" || c.Title == "" || c.Doc == "":
		return fmt.Errorf("name, title and doc are required")
	case len(c.When) < 2:
		return fmt.Errorf("when needs two or more conditions; one condition is just that label's own band")
	}
	for _, w := range c.When {
		if w.Label == "" || w.AtLeast == "" {
			return fmt.Errorf("every condition needs a label and an at_least band")
		}
		defs := label.Definitions(w.Label)
		if len(defs) == 0 {
			return fmt.Errorf("when names %s, which is not a label", w.Label)
		}
		known := false
		for _, d := range defs {
			if d.HasLevel(w.AtLeast) {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("when names %s at_least %s, which is not one of its bands", w.Label, w.AtLeast)
		}
	}
	return nil
}
