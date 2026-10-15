// Package shell reads the printouts a collection script leaves beside a bundle's logs: the output
// of serverStatus(), rs.status(), hostInfo() and the like, as a mongo shell printed them.
//
// A shell printout is not JSON. It carries unquoted keys and the shell's own constructors —
// Long('12'), ObjectId('...'), ISODate('...') — so it has to be converted before anything can read
// it, which is what this package is for.
package shell

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Constructors mongosh prints around a value. Each is replaced by the JSON the value would have
// been written as, and a type that has no JSON of its own keeps its name inside the string so a
// reader can still see what it was.
var (
	longValue      = regexp.MustCompile(`(?:Number)?Long\(['"]?(-?\d+)['"]?\)`)
	intValue       = regexp.MustCompile(`(?:Number)?Int\(['"]?(-?\d+)['"]?\)`)
	decimalValue   = regexp.MustCompile(`NumberDecimal\('([^']*)'\)`)
	doubleValue    = regexp.MustCompile(`Double\(['"]?(-?[\d.eE+-]+)['"]?\)`)
	objectIDValue  = regexp.MustCompile(`ObjectId\('([^']*)'\)`)
	uuidValue      = regexp.MustCompile(`UUID\('([^']*)'\)`)
	dateValue      = regexp.MustCompile(`ISODate\('([^']*)'\)`)
	binaryValue    = regexp.MustCompile(`Binary\.createFromBase64\('([^']*)',\s*\d+\)`)
	timestampValue = regexp.MustCompile(`Timestamp\(\{\s*t:\s*(\d+),\s*i:\s*(\d+)\s*\}\)`)
	bareKey        = regexp.MustCompile(`([{,]\s*)([a-zA-Z_$][a-zA-Z0-9_$]*)(\s*:)`)
	trailingComma  = regexp.MustCompile(`([,\[\{])\s*,(\s*[}\]])`)
)

// ConvertEJSONToJSON turns a shell printout into JSON a decoder will take.
//
// The constructors go first, because they are written with single quotes inside them and the pass
// that converts the quotes would otherwise take their arguments apart. Then the bare keys, which
// only the shell writes unquoted. Then the strings themselves: mongosh prints every string in
// single quotes, and a converter that left them was the difference between reading a bundle's
// rs.status() and reporting it as unparsable.
func ConvertEJSONToJSON(ejson string) ([]byte, error) {
	text := ejson

	text = longValue.ReplaceAllString(text, "$1")
	text = intValue.ReplaceAllString(text, "$1")
	text = doubleValue.ReplaceAllString(text, "$1")
	text = decimalValue.ReplaceAllString(text, `"$1"`)
	text = objectIDValue.ReplaceAllString(text, `"ObjectId($1)"`)
	text = uuidValue.ReplaceAllString(text, `"UUID($1)"`)
	text = dateValue.ReplaceAllString(text, `"$1"`)
	text = binaryValue.ReplaceAllString(text, `"Binary($1)"`)
	text = timestampValue.ReplaceAllString(text, `"Timestamp($1, $2)"`)
	text = bareKey.ReplaceAllString(text, `$1"$2"$3`)
	text = trailingComma.ReplaceAllString(text, `$1$2`)
	text = doubleQuote(text)

	return []byte(text), nil
}

// doubleQuote rewrites the shell's single-quoted strings as JSON ones, and joins the pieces of a
// string the shell split across lines.
//
// It is a scan rather than a pattern because the two kinds of quote nest: a single-quoted string
// may hold a double quote and a double-quoted one may hold an apostrophe, and a regular expression
// that cannot tell which it is inside turns an apostrophe in a message into unparsable text.
//
// The joining is for tcmalloc. mongosh prints its formatted heap report as a dozen string literals
// with + between them, exactly as JavaScript source, and JSON has no concatenation: left alone, one
// field of serverStatus made the whole document unreadable.
func doubleQuote(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] != '"' && text[i] != '\'' {
			out.WriteByte(text[i])
			i++
			continue
		}
		content, next := readString(text, i)
		// Whatever follows a + is the rest of the same string.
		for {
			after := skipJoin(text, next)
			if after < 0 {
				break
			}
			more, end := readString(text, after)
			content, next = content+more, end
		}
		quoted, err := json.Marshal(content)
		if err != nil {
			out.WriteString(`""`)
		} else {
			out.Write(quoted)
		}
		i = next
	}
	return out.String()
}

// readString reads one quoted string of either kind and returns its contents and the index just
// past its closing quote.
func readString(text string, start int) (string, int) {
	quote := text[start]
	var content strings.Builder
	for i := start + 1; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			i++
			// The shell escapes its own quote, which JSON does not need escaped; everything else
			// keeps the escape, because JSON spells \n and \t the same way.
			if text[i] != quote {
				content.WriteByte('\\')
			}
			content.WriteByte(text[i])
			continue
		}
		if text[i] == quote {
			return content.String(), i + 1
		}
		content.WriteByte(text[i])
	}
	return content.String(), len(text)
}

// skipJoin reports where the next piece of a concatenated string begins, or -1 when what follows is
// not a concatenation.
func skipJoin(text string, from int) int {
	i := from
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	if i >= len(text) || text[i] != '+' {
		return -1
	}
	for i++; i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r'); i++ {
	}
	if i < len(text) && (text[i] == '"' || text[i] == '\'') {
		return i
	}
	return -1
}

// ParseEJSON parses EJSON text and returns a map
func ParseEJSON(text string) (map[string]interface{}, error) {
	jsonBytes, err := ConvertEJSONToJSON(text)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	err = json.Unmarshal(jsonBytes, &result)
	return result, err
}

// GetString safely gets a string value from nested map
func GetString(m map[string]interface{}, path ...string) string {
	current := interface{}(m)
	for _, key := range path {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[key]
		default:
			return ""
		}
	}
	if s, ok := current.(string); ok {
		return s
	}
	return ""
}

// GetInt safely gets an int value from nested map
func GetInt(m map[string]interface{}, path ...string) int {
	current := interface{}(m)
	for _, key := range path {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[key]
		default:
			return 0
		}
	}
	switch v := current.(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// GetMap safely gets a map value from nested map
func GetMap(m map[string]interface{}, path ...string) map[string]interface{} {
	current := interface{}(m)
	for _, key := range path {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[key]
		default:
			return nil
		}
	}
	if result, ok := current.(map[string]interface{}); ok {
		return result
	}
	return nil
}

// GetArray safely gets an array value from nested map
func GetArray(m map[string]interface{}, path ...string) []interface{} {
	current := interface{}(m)
	for _, key := range path {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[key]
		default:
			return nil
		}
	}
	if result, ok := current.([]interface{}); ok {
		return result
	}
	return nil
}
