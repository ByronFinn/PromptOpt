package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// NormalizeInput canonicalizes a sample input for identity comparison
// (V7 提案 §1.2 锚点库与 §3.3 跨 run 候选池共用——复审确认 P5/P8 各自实现
// 会漂移，导致跨 run 比对系统性失配，故唯一实现落在本共享模型包):
//
//  1. leading/trailing whitespace is trimmed;
//  2. every internal whitespace run (spaces, tabs, newlines, … any
//     unicode.IsSpace rune) collapses to a single ASCII space.
//
// 即「TrimSpace + 换行归一」的加强版：两类差异在 LLM 数据集中几乎总是
// 无意义的排版噪音，而 collapsing to one canonical form 让
// "same text, different whitespace layout" 哈希到同一条目。语义内容
// （标点、大小写、字序）保持敏感。后续演进只改这一处。
func NormalizeInput(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// HashInput returns the lowercase hex sha256 of the normalized input —
// the dedup key of the anchor library (anchors.Entry.InputHash) and the
// default task-key material for zero-config runs. Callers must derive
// the hash through this function only; a parallel implementation would
// break cross-run and cross-package comparison.
func HashInput(s string) string {
	sum := sha256.Sum256([]byte(NormalizeInput(s)))
	return hex.EncodeToString(sum[:])
}
