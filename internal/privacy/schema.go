package privacy

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

var (
	identRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	piiLikeRe   = regexp.MustCompile(`(?i)(email|phone|msisdn|address)|(^|_)(name|ip)(_|$)`)
	createTblRe = regexp.MustCompile(`(?i)CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	dropTblRe   = regexp.MustCompile(`(?i)DROP\s+TABLE(?:\s+IF\s+EXISTS)?\s+([A-Za-z_][A-Za-z0-9_]*)`)
	alterTblRe  = regexp.MustCompile(`(?i)ALTER\s+TABLE(?:\s+IF\s+EXISTS)?\s+([A-Za-z_][A-Za-z0-9_]*)`)
	addColRe    = regexp.MustCompile(`(?i)ADD\s+COLUMN(?:\s+IF\s+NOT\s+EXISTS)?\s+([A-Za-z_][A-Za-z0-9_]*)`)
	dropColRe   = regexp.MustCompile(`(?i)DROP\s+COLUMN(?:\s+IF\s+EXISTS)?\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

func stripSQLComments(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break
			}
			i += 2 + j + 2
			continue
		}
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func splitDefs(body string) []string {
	var parts []string
	var buf strings.Builder
	depth := 0
	for _, ch := range body {
		switch ch {
		case '(':
			depth++
			buf.WriteRune(ch)
		case ')':
			depth--
			buf.WriteRune(ch)
		case ',':
			if depth == 0 {
				if s := strings.TrimSpace(buf.String()); s != "" {
					parts = append(parts, s)
				}
				buf.Reset()
			} else {
				buf.WriteRune(ch)
			}
		default:
			buf.WriteRune(ch)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}

func colsFromCreate(body string) []string {
	var cols []string
	for _, part := range splitDefs(body) {
		up := strings.ToUpper(strings.TrimSpace(part))
		if strings.HasPrefix(up, "PRIMARY KEY") || strings.HasPrefix(up, "UNIQUE") ||
			strings.HasPrefix(up, "CONSTRAINT") || strings.HasPrefix(up, "CHECK") ||
			strings.HasPrefix(up, "FOREIGN KEY") || strings.HasPrefix(up, "EXCLUDE") {
			continue
		}
		m := identRe.FindString(part)
		if m != "" {
			cols = append(cols, strings.ToLower(m))
		}
	}
	return cols
}

func extractCreateBody(sql string, openParen int) (body string, end int) {
	depth := 1
	i := openParen + 1
	for i < len(sql) && depth > 0 {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		i++
	}
	if depth != 0 {
		return "", openParen
	}
	return sql[openParen+1 : i-1], i
}

func statements(sql string) []string {
	var out []string
	var buf strings.Builder
	depth := 0
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch ch {
		case '(':
			depth++
			buf.WriteByte(ch)
		case ')':
			if depth > 0 {
				depth--
			}
			buf.WriteByte(ch)
		case ';':
			if depth == 0 {
				if s := strings.TrimSpace(buf.String()); s != "" {
					out = append(out, s)
				}
				buf.Reset()
			} else {
				buf.WriteByte(ch)
			}
		default:
			buf.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// FinalSchemaColumns walks migrations in order and returns table.column for
// columns that exist after the full chain.
func FinalSchemaColumns(fsys fs.FS) (map[string]map[string]bool, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	schema := map[string]map[string]bool{}
	ensure := func(t string) map[string]bool {
		t = strings.ToLower(t)
		if schema[t] == nil {
			schema[t] = map[string]bool{}
		}
		return schema[t]
	}
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		sql := stripSQLComments(string(raw))
		for _, stmt := range statements(sql) {
			if m := createTblRe.FindStringSubmatchIndex(stmt); m != nil {
				table := strings.ToLower(stmt[m[2]:m[3]])
				open := strings.IndexByte(stmt[m[1]-1:], '(')
				if open < 0 {
					continue
				}
				body, _ := extractCreateBody(stmt, m[1]-1+open)
				cols := ensure(table)
				for _, c := range colsFromCreate(body) {
					cols[c] = true
				}
				continue
			}
			if m := dropTblRe.FindStringSubmatch(stmt); m != nil {
				delete(schema, strings.ToLower(m[1]))
				continue
			}
			if m := alterTblRe.FindStringSubmatch(stmt); m != nil {
				table := strings.ToLower(m[1])
				cols := ensure(table)
				for _, cm := range addColRe.FindAllStringSubmatch(stmt, -1) {
					cols[strings.ToLower(cm[1])] = true
				}
				for _, cm := range dropColRe.FindAllStringSubmatch(stmt, -1) {
					delete(cols, strings.ToLower(cm[1]))
				}
			}
		}
	}
	return schema, nil
}

// PIILikeKeys returns table.column for final-schema columns whose names look
// like email, phone, name, address, ip or msisdn.
func PIILikeKeys(fsys fs.FS) ([]string, error) {
	schema, err := FinalSchemaColumns(fsys)
	if err != nil {
		return nil, err
	}
	var keys []string
	for table, cols := range schema {
		for col := range cols {
			if piiLikeRe.MatchString(col) {
				keys = append(keys, table+"."+col)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}
