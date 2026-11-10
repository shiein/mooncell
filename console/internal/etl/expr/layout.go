package expr

import "strings"

// GoLayout 把 Kettle / Java 风格的日期格式（yyyy-MM-dd HH:mm:ss.SSS）转换为 Go layout。
// 单引号包裹的文本按字面量保留（如 yyyy-MM-dd'T'HH:mm:ss）。
func GoLayout(java string) string {
	var b strings.Builder
	rs := []rune(java)
	for i := 0; i < len(rs); {
		c := rs[i]
		if c == '\'' {
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			b.WriteString(string(rs[i+1 : min(j, len(rs))]))
			i = j + 1
			continue
		}
		n := 1
		for i+n < len(rs) && rs[i+n] == c {
			n++
		}
		b.WriteString(token(c, n))
		i += n
	}
	return b.String()
}

func token(c rune, n int) string {
	switch c {
	case 'y':
		if n == 2 {
			return "06"
		}
		return "2006"
	case 'M':
		switch {
		case n >= 4:
			return "January"
		case n == 3:
			return "Jan"
		case n == 2:
			return "01"
		}
		return "1"
	case 'd':
		if n >= 2 {
			return "02"
		}
		return "2"
	case 'H':
		return "15"
	case 'h':
		if n >= 2 {
			return "03"
		}
		return "3"
	case 'm':
		if n >= 2 {
			return "04"
		}
		return "4"
	case 's':
		if n >= 2 {
			return "05"
		}
		return "5"
	case 'S':
		return strings.Repeat("0", n)
	case 'a':
		return "PM"
	case 'E':
		if n >= 4 {
			return "Monday"
		}
		return "Mon"
	case 'Z':
		return "-0700"
	case 'X':
		if n >= 3 {
			return "-07:00"
		}
		return "-07"
	}
	return strings.Repeat(string(c), n)
}
