package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// nonPersianChars matches characters that have no business in a Persian
// recipe post: CJK, Hangul, Kana, Latin letters, and the Arabic-only
// glyphs the prompt explicitly bans (ك ي ة ى — Persian uses ک ی ـه ی).
// Digits and punctuation are intentionally allowed.
var nonPersianChars = regexp.MustCompile(`[\x{4E00}-\x{9FFF}\x{AC00}-\x{D7AF}\x{3040}-\x{30FF}A-Za-z\x{0643}\x{064A}\x{0629}\x{0649}]`)

// formalPhrases are written/book-Persian constructions the channel voice
// avoids. The brief asks for conversational ("استاد بابک") tone.
var formalPhrases = []string{
	"خود را", "این را", "آن را", "می‌باشد", "می باشد",
	"گردید", "نمایید", "بفرمایید", "غوطه‌ور",
}

// inventedWords is a deny-list of fabricated tokens that weaker models
// hallucinate when they don't know a real Persian cooking term. Keep
// entries narrowly literal — every entry must be a string that has no
// legitimate use in a Persian recipe, or it will block valid posts.
var inventedWords = []string{
	"ملاوه", "هم‌ریخت", "بروش", "برسش",
	"گران‌پایه", "یخ‌پاک", "تشویه", "حول‌مایه",
	"انگشتی لیدی",
}

type ValidationKind string

const (
	IssueNonPersian ValidationKind = "non-persian"
	IssueFormal     ValidationKind = "formal"
	IssueInvented   ValidationKind = "invented"
)

type ValidationIssue struct {
	Kind    ValidationKind
	Snippet string
}

// Validate scans the provided Persian text fields and returns every issue
// found. Pass each user-visible string separately (title, intro, each
// ingredient, each step, tip) so the same snippet matched in two fields
// isn't reported twice.
func Validate(texts ...string) []ValidationIssue {
	var issues []ValidationIssue
	seen := map[string]bool{}
	add := func(kind ValidationKind, snippet string) {
		key := string(kind) + "|" + snippet
		if seen[key] {
			return
		}
		seen[key] = true
		issues = append(issues, ValidationIssue{Kind: kind, Snippet: snippet})
	}
	for _, t := range texts {
		for _, m := range nonPersianChars.FindAllString(t, -1) {
			add(IssueNonPersian, m)
		}
		for _, p := range formalPhrases {
			if strings.Contains(t, p) {
				add(IssueFormal, p)
			}
		}
		for _, w := range inventedWords {
			if strings.Contains(t, w) {
				add(IssueInvented, w)
			}
		}
	}
	return issues
}

// FormatRetryRequest builds a Persian user-side message asking the model
// to patch the listed issues and return a corrected JSON. The previous
// JSON is embedded verbatim so the model edits it in place rather than
// regenerating from scratch.
func FormatRetryRequest(prevJSON string, issues []ValidationIssue) string {
	var b strings.Builder
	b.WriteString("خروجی JSON قبلیت این بود:\n")
	b.WriteString(prevJSON)
	b.WriteString("\n\nاین مشکلات رو داره، فقط همین‌ها رو اصلاح کن:\n")
	for _, i := range issues {
		switch i.Kind {
		case IssueNonPersian:
			fmt.Fprintf(&b, "- کاراکتر یا کلمه‌ی غیرفارسی: «%s» — حذفش کن یا با معادل فارسی عوضش کن.\n", i.Snippet)
		case IssueFormal:
			fmt.Fprintf(&b, "- عبارت رسمی/کتابی: «%s» — محاوره‌ش کن.\n", i.Snippet)
		case IssueInvented:
			fmt.Fprintf(&b, "- کلمه‌ی ساختگی: «%s» — با کلمه‌ی واقعی فارسی عوضش کن.\n", i.Snippet)
		}
	}
	b.WriteString("\nفقط JSON اصلاح‌شده رو برگردون — بدون توضیح، بدون code fence.")
	return b.String()
}
