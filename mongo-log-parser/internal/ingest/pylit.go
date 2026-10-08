package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pyPrefix converts the one value text opens with and returns what follows it.
func pyPrefix(text string) (string, string, error) {
	p := &pyParser{s: text}
	var b strings.Builder
	p.skipSpace()
	if err := p.value(&b); err != nil {
		return "", "", err
	}
	return b.String(), text[p.i:], nil
}

type pyParser struct {
	s string
	i int
}

func (p *pyParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\n' || p.s[p.i] == '\t' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *pyParser) value(b *strings.Builder) error {
	if p.i >= len(p.s) {
		return fmt.Errorf("unexpected end")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.container(b, '{', '}', true)
	case c == '[':
		return p.container(b, '[', ']', false)
	case c == '(':
		return p.container(b, '(', ')', false)
	case c == '\'' || c == '"':
		str, err := p.str()
		if err != nil {
			return err
		}
		out, _ := json.Marshal(str)
		b.Write(out)
		return nil
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return p.number(b)
	default:
		return p.word(b)
	}
}

func (p *pyParser) container(b *strings.Builder, open, close byte, dict bool) error {
	p.i++
	if dict {
		b.WriteByte('{')
	} else {
		b.WriteByte('[')
	}
	first := true
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return fmt.Errorf("unclosed %c", open)
		}
		if p.s[p.i] == close {
			p.i++
			break
		}
		if !first {
			if p.s[p.i] != ',' {
				return fmt.Errorf("expected , at %d", p.i)
			}
			p.i++
			p.skipSpace()
			if p.i < len(p.s) && p.s[p.i] == close {
				p.i++
				break
			}
		}
		first = false
		if dict {
			// A key is a string in every attr we have seen; anything else is written as its text.
			var key string
			if p.s[p.i] == '\'' || p.s[p.i] == '"' {
				k, err := p.str()
				if err != nil {
					return err
				}
				key = k
			} else {
				start := p.i
				for p.i < len(p.s) && p.s[p.i] != ':' {
					p.i++
				}
				key = strings.TrimSpace(p.s[start:p.i])
			}
			out, _ := json.Marshal(key)
			b.Write(out)
			p.skipSpace()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return fmt.Errorf("expected : at %d", p.i)
			}
			p.i++
			b.WriteByte(':')
			p.skipSpace()
		}
		if err := p.value(b); err != nil {
			return err
		}
		b.WriteByte(',')
	}
	out := b.String()
	if strings.HasSuffix(out, ",") {
		trimmed := out[:len(out)-1]
		b.Reset()
		b.WriteString(trimmed)
	}
	if dict {
		b.WriteByte('}')
	} else {
		b.WriteByte(']')
	}
	return nil
}

func (p *pyParser) str() (string, error) {
	quote := p.s[p.i]
	p.i++
	var out strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == quote {
			p.i++
			return out.String(), nil
		}
		if c != '\\' {
			r, size := utf8.DecodeRuneInString(p.s[p.i:])
			out.WriteRune(r)
			p.i += size
			continue
		}
		p.i++
		if p.i >= len(p.s) {
			break
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case '0':
			out.WriteByte(0)
		case 'x', 'u', 'U':
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if p.i+width > len(p.s) {
				return "", fmt.Errorf("short escape")
			}
			code, err := strconv.ParseUint(p.s[p.i:p.i+width], 16, 32)
			if err != nil {
				return "", err
			}
			out.WriteRune(rune(code))
			p.i += width
		default:
			out.WriteByte(e)
		}
	}
	return "", fmt.Errorf("unclosed string")
}

func (p *pyParser) number(b *strings.Builder) error {
	start := p.i
	for p.i < len(p.s) && strings.IndexByte("+-.0123456789eE", p.s[p.i]) >= 0 {
		p.i++
	}
	text := p.s[start:p.i]
	if _, err := strconv.ParseFloat(text, 64); err != nil {
		b.WriteString(strconv.Quote(text))
		return nil
	}
	b.WriteString(strings.TrimPrefix(text, "+"))
	return nil
}

// word is a bare name: True, False, None, inf, nan, or a constructor call kept as text.
func (p *pyParser) word(b *strings.Builder) error {
	start := p.i
	for p.i < len(p.s) && (isIdent(p.s[p.i]) || p.s[p.i] == '.') {
		p.i++
	}
	name := p.s[start:p.i]
	if p.i < len(p.s) && p.s[p.i] == '(' {
		depth := 0
		for p.i < len(p.s) {
			switch p.s[p.i] {
			case '(':
				depth++
			case ')':
				depth--
			case '\'', '"':
				if _, err := p.str(); err != nil {
					return err
				}
				continue
			}
			p.i++
			if depth == 0 {
				break
			}
		}
		out, _ := json.Marshal(p.s[start:p.i])
		b.Write(out)
		return nil
	}
	switch name {
	case "True":
		b.WriteString("true")
	case "False":
		b.WriteString("false")
	case "None":
		b.WriteString("null")
	case "":
		return fmt.Errorf("unexpected %q at %d", p.s[p.i], p.i)
	default:
		out, _ := json.Marshal(name)
		b.Write(out)
	}
	return nil
}

func isIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
