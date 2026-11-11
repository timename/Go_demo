package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestValidateQuestionChinese 验证中文长度按字符数判断
func TestValidateQuestionChinese(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"正常中文", "我的血糖怎么样？", false},
		{"700 中文字", strings.Repeat("血", 700), false},   // 2100 字节，但只有 700 字符
		{"2000 中文字", strings.Repeat("血", 2000), false}, // 边界内
		{"2001 中文字", strings.Repeat("血", 2001), true},  // 超限
		{"空字符串", "", true},
		{"只有空格", "   ", true}, // normalize 后为空
	}

	for _, c := range cases {
		q := normalizeQuestion(c.input)
		err := validateQuestion(q)
		bytes := len(q)
		chars := utf8.RuneCountInString(q)

		if (err != nil) != c.wantErr {
			t.Errorf("【失败】%s: 期望错误=%v，实际=%v (bytes=%d chars=%d)",
				c.name, c.wantErr, err, bytes, chars)
			continue
		}
		if c.wantErr && err != nil {
			t.Logf("【通过】%-14s 字节=%-6d 字符=%-5d 错误=%v", c.name, bytes, chars, err)
		} else {
			t.Logf("【通过】%-14s 字节=%-6d 字符=%-5d 无错误", c.name, bytes, chars)
		}
	}
}

// TestNormalizeQuestion 验证输入清洗（含全角空格）
func TestNormalizeQuestion(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"  hello  world  ", "hello world"},
		{"血糖\n怎么样", "血糖 怎么样"},
		{"全角空格：\u3000血糖\u3000", "全角空格： 血糖"}, // 全角空格应被处理
		{"tab\t分隔", "tab 分隔"},
		{"连续    多个空格", "连续 多个空格"},
	}

	for _, c := range cases {
		got := normalizeQuestion(c.input)
		if got != c.want {
			t.Errorf("【失败】输入 %q\n  期望 %q\n  实际 %q", c.input, c.want, got)
			continue
		}
		t.Logf("【通过】%q -> %q", c.input, got)
	}
}

// TestCharLen 验证字符计数
func TestCharLen(t *testing.T) {
	cases := []struct {
		input     string
		wantChars int
	}{
		{"血糖", 2},
		{"hello", 5},
		{"血糖 hello", 8},
		{"", 0},
	}
	for _, c := range cases {
		if got := charLen(c.input); got != c.wantChars {
			t.Errorf("【失败】charLen(%q)=%d，期望 %d", c.input, got, c.wantChars)
			continue
		}
		t.Logf("【通过】charLen(%q)=%d (字节=%d)", c.input, c.wantChars, len(c.input))
	}
}
