package store

import "strings"

// Field finds the value of a top-level key in a JSON object without decoding the object: the
// last occurrence whose key equals name, ignoring case, the way encoding/json picks a struct
// field. ok is false when the text is not an object this scanner can walk; callers fall back to
// encoding/json then.
//
// A log line is mostly its attributes, and pulling them out with encoding/json read the whole line
// three times over, once for every analyzer that asked.
func Field(object, name string) (value string, found, ok bool) {
	values, ok := Fields(object, name)
	return values[0], values[0] != "", ok
}

// Fields is Field for several names in one pass; a name not present has an empty value.
func Fields(object string, names ...string) ([]string, bool) {
	values := make([]string, len(names))
	fail := func() ([]string, bool) { return make([]string, len(names)), false }
	i := skipSpace(object, 0)
	if i >= len(object) || object[i] != '{' {
		return fail()
	}
	i = skipSpace(object, i+1)
	if i < len(object) && object[i] == '}' {
		return values, true
	}
	for i < len(object) {
		keyEnd, good := skipString(object, i)
		if !good {
			return fail()
		}
		key := object[i+1 : keyEnd-1]
		i = skipSpace(object, keyEnd)
		if i >= len(object) || object[i] != ':' {
			return fail()
		}
		i = skipSpace(object, i+1)
		end, good := skipValue(object, i)
		if !good {
			return fail()
		}
		// An escaped key is compared as written; encoding/json would unescape it first, so such a
		// key sends the caller back to it.
		if strings.IndexByte(key, '\\') >= 0 {
			return fail()
		}
		for n, name := range names {
			if strings.EqualFold(key, name) {
				values[n] = object[i:end]
			}
		}
		i = skipSpace(object, end)
		if i < len(object) && object[i] == ',' {
			i = skipSpace(object, i+1)
			continue
		}
		if i < len(object) && object[i] == '}' {
			return values, true
		}
		return fail()
	}
	return fail()
}

func skipSpace(text string, i int) int {
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string starting at i.
func skipString(text string, i int) (int, bool) {
	if i >= len(text) || text[i] != '"' {
		return 0, false
	}
	for i++; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

// skipValue returns the index after the value starting at i.
func skipValue(text string, i int) (int, bool) {
	if i >= len(text) {
		return 0, false
	}
	switch text[i] {
	case '"':
		return skipString(text, i)
	case '{', '[':
		depth := 0
		for ; i < len(text); i++ {
			switch text[i] {
			case '"':
				end, ok := skipString(text, i)
				if !ok {
					return 0, false
				}
				i = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default:
		start := i
		for i < len(text) && !strings.ContainsRune(",}] \t\n\r", rune(text[i])) {
			i++
		}
		return i, i > start
	}
}
