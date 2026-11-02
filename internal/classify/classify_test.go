package classify

import (
	"strings"
	"testing"
)

func TestClassifiersLoadAndValidate(t *testing.T) {
	if _, err := load(); err != nil {
		t.Fatal(err)
	}
	if len(Classifiers) < 2 {
		t.Fatalf("%d classifiers loaded", len(Classifiers))
	}
}

// A classifier a reviewer gets wrong is refused when the binary starts, not discovered on a bundle.
func TestMalformedClassifiersAreRefused(t *testing.T) {
	base := func() Classifier {
		return Classifier{Name: "x", Title: "x", Doc: "x", When: []Condition{
			{Label: "cpu.busy", AtLeast: "high"},
			{Label: "disk.busy", AtLeast: "high"},
		}}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("the base classifier is valid: %v", err)
	}
	cases := map[string]func(*Classifier){
		"name, title and doc":    func(c *Classifier) { c.Name = "" },
		"two or more conditions": func(c *Classifier) { c.When = c.When[:1] },
		"not a label":            func(c *Classifier) { c.When[0].Label = "not.a.real.label" },
		"not one of its bands":   func(c *Classifier) { c.When[0].AtLeast = "not_a_band" },
	}
	for want, breakIt := range cases {
		c := base()
		breakIt(&c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error containing %q, got %v", want, want, err)
		}
	}
}
