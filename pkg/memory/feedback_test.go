package memory

import (
	"testing"
)

func TestIsNegativeFeedback_Chinese(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"direct_correction_budui", "不对", true},
		{"direct_correction_cuole", "错了", true},
		{"direct_correction_gaocuole", "搞错了", true},
		{"direct_correction_buzheyang", "不是这样", true},
		{"direct_correction_bushiyaode", "不是我要的", true},
		{"retry_woyisi", "我的意思是", true},
		{"retry_huanhuashuo", "我换个说法试试", true},
		{"retry_woshideshi", "我说的是", true},
		{"dissatisfaction_meiyong", "没用", true},
		{"dissatisfaction_kanbudong", "看不懂", true},
		{"dissatisfaction_butaidui", "不太对", true},
		{"dissatisfaction_dafasuowen", "答非所问", true},
		{"redo_chongxinshengcheng", "重新生成", true},
		{"redo_chongzuo", "重做一下", true},
		{"redo_chelai", "撤回", true},
		{"neutral_continue", "继续，然后加上错误处理", false},
		{"neutral_long", "这个方案可以，帮我实现一下", false},
		{"neutral_short", "好的", false},
		{"english_wrong", "That's wrong, please fix it", true},
		{"english_rephrase", "let me rephrase that", true},
		{"english_dissatisfied", "this doesn't help", true},
		{"english_redo", "redo this part", true},
		{"chinese_in_sentence", "这个逻辑不太对，帮我检查一下", true},
		{"mixed_case", "不对，let me rephrase", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsNegativeFeedback(tt.msg)
			if got != tt.want {
				t.Errorf("IsNegativeFeedback(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestClassifyUserResponse(t *testing.T) {
	tests := []struct {
		name       string
		msg        string
		prevMsg    string
		similarity float64
		want       FeedbackClassification
	}{
		{"empty", "", "", 0, FeedbackNeutral},
		{"too_short", "ok", "prev", 0, FeedbackNeutral},
		{"exactly_min", "1234567890", "prev", 0, FeedbackPositive},
		{"just_above_min", "12345678901", "prev", 0, FeedbackPositive},
		{"negative_correction", "不对，这是错的", "prev", 0, FeedbackNegative},
		{"negative_short", "不对", "", 0, FeedbackNegative},
		{"retry_pattern", "换个说法试试", "prev", 0, FeedbackNegative},
		{"high_similarity_retry", "帮我看看这个文件的内容帮我看看这个文件的内容", "prev", 0.9, FeedbackNeutral},
		{"normal_positive", "继续帮我实现这个功能", "prev", 0.1, FeedbackPositive},
		{"negative_then_positive", "这个方案可以，继续吧", "不对", 0.1, FeedbackPositive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyUserResponse(tt.msg, tt.prevMsg, tt.similarity).Classification
			if got != tt.want {
				t.Errorf("ClassifyUserResponse(%q, %q, %.2f) = %v, want %v",
					tt.msg, tt.prevMsg, tt.similarity, got, tt.want)
			}
		})
	}
}

func TestClassifyUserResponse_MinPositiveLengthBoundary(t *testing.T) {
	// Test exact boundary: length 9 should be neutral, 10 should be positive.
	short := make([]rune, MinPositiveLength-1)
	for i := range short {
		short[i] = 'a'
	}
	result := ClassifyUserResponse(string(short), "prev", 0)
	if result.Classification != FeedbackNeutral {
		t.Errorf("length %d should be neutral, got %v", MinPositiveLength-1, result.Classification)
	}

	exact := make([]rune, MinPositiveLength)
	for i := range exact {
		exact[i] = 'a'
	}
	result = ClassifyUserResponse(string(exact), "prev", 0)
	if result.Classification != FeedbackPositive {
		t.Errorf("length %d should be positive, got %v", MinPositiveLength, result.Classification)
	}
}

func TestTextCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want float64
	}{
		{"identical", "hello world", "hello world", 1.0},
		{"disjoint", "hello world", "foo bar baz", 0},
		{"partial", "hello world foo", "hello world bar", 0.667},
		{"empty_a", "", "hello", 0},
		{"empty_b", "hello", "", 0},
		{"both_empty", "", "", 0},
		{"cjk_identical", "你好世界", "你好世界", 1.0},
		{"cjk_disjoint", "你好世界", "再见明天", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TextCosineSimilarity(tt.a, tt.b)
			if tt.want == 1.0 && got < 0.9999 {
				t.Errorf("TextCosineSimilarity(%q, %q) = %v, want 1.0", tt.a, tt.b, got)
			} else if tt.want == 0 && got > 0.01 {
				t.Errorf("TextCosineSimilarity(%q, %q) = %v, want ~0", tt.a, tt.b, got)
			}
		})
	}
}
