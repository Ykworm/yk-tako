package tools

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Eval 只接受数字和 + - * / % ( )。拒绝任意代码，和 TypeScript 版的白名单同一条边界。
func Eval(expression string) (string, error) {
	for _, r := range expression {
		if unicode.IsSpace(r) {
			continue
		}
		if unicode.IsDigit(r) || strings.ContainsRune("+-*/().%", r) {
			continue
		}
		return "", fmt.Errorf("表达式只能包含数字和 + - * / ( ) %% 运算符")
	}
	p := &parser{s: expression}
	value, err := p.parseExpr()
	if err != nil {
		return "", fmt.Errorf("无法计算的表达式: %s", expression)
	}
	p.skip()
	if p.i != len(p.s) {
		return "", fmt.Errorf("无法计算的表达式: %s", expression)
	}
	return formatNumber(value), nil
}

func formatNumber(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

type parser struct {
	s string
	i int
}

func (p *parser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *parser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skip()
		if p.i >= len(p.s) {
			return left, nil
		}
		op := p.s[p.i]
		if op != '+' && op != '-' {
			return left, nil
		}
		p.i++
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *parser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		p.skip()
		if p.i >= len(p.s) {
			return left, nil
		}
		op := p.s[p.i]
		if op != '*' && op != '/' && op != '%' {
			return left, nil
		}
		p.i++
		right, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		switch op {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, fmt.Errorf("除以 0")
			}
			left /= right
		case '%':
			if right == 0 {
				return 0, fmt.Errorf("除以 0")
			}
			left = float64(int64(left) % int64(right))
		}
	}
}

func (p *parser) parseUnary() (float64, error) {
	p.skip()
	if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
		op := p.s[p.i]
		p.i++
		value, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if op == '-' {
			return -value, nil
		}
		return value, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (float64, error) {
	p.skip()
	if p.i >= len(p.s) {
		return 0, fmt.Errorf("表达式意外结束")
	}
	if p.s[p.i] == '(' {
		p.i++
		value, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return 0, fmt.Errorf("缺少右括号")
		}
		p.i++
		return value, nil
	}
	start := p.i
	if p.s[p.i] == '.' {
		p.i++
	}
	sawDigit := false
	for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.') {
		if p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			sawDigit = true
		}
		p.i++
	}
	if !sawDigit || start == p.i {
		return 0, fmt.Errorf("缺少数字")
	}
	return strconv.ParseFloat(p.s[start:p.i], 64)
}
