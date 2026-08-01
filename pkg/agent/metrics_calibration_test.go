package agent

import (
	"fmt"
	"testing"
)

// TestCalibrateBytesPerToken 测试不同的 bytes/token 比率
// 基于理论分析：代码主导内容 (58.7%) + JSON 参数 (28.9%) + 中英文混合 (12.4%)
// 理论加权平均值: 3.3 bytes/token
func TestCalibrateBytesPerToken(t *testing.T) {
	// 基于实际数据的测试用例
	tests := []struct {
		name           string
		contextBytes   int
		expectedTokens map[float64]int // 不同比率下的预期 tokens
	}{
		{
			name:         "典型工具调用场景 (200KB 工具结果)",
			contextBytes: 204491, // 平均工具结果字节数
			expectedTokens: map[float64]int{
				3.0: 68164, // 当前保守估计
				3.3: 61967, // 理论计算值
				3.5: 58426, // 代码优化值
				4.0: 51123, // 纯英文假设
			},
		},
		{
			name:         "JSON 参数场景 (100KB)",
			contextBytes: 100751, // 平均 AI args 字节数
			expectedTokens: map[float64]int{
				3.0: 33584,
				3.3: 30530,
				3.5: 28786,
				4.0: 25188,
			},
		},
		{
			name:         "AI 回复场景 (38KB)",
			contextBytes: 38589, // 平均 AI content 字节数
			expectedTokens: map[float64]int{
				3.0: 12863,
				3.3: 11694,
				3.5: 11025,
				4.0: 9647,
			},
		},
		{
			name:         "完整上下文场景 (348KB 总计)",
			contextBytes: 348496, // 平均总字节数
			expectedTokens: map[float64]int{
				3.0: 116165,
				3.3: 105605,
				3.5: 99570,
				4.0: 87124,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for ratio, expected := range tt.expectedTokens {
				t.Run(fmt.Sprintf("%.1f", ratio), func(t *testing.T) {
					// 使用指定比率计算，允许 ±1 误差
					estimated := int(float64(tt.contextBytes) / ratio)
					if estimated < expected-1 || estimated > expected+1 {
						t.Errorf("比率 %.1f: 计算得到 %d tokens, 期望 %d (±1)", ratio, estimated, expected)
					}
				})
			}
		})
	}
}

func ratioToString(r float64) string {
	return fmt.Sprintf("%.1f", r)
}

// TestOptimalBytesPerToken 测试最优比率选择
// 基于内容构成：代码 58.7% + JSON 28.9% + 文本 12.4%
func TestOptimalBytesPerToken(t *testing.T) {
	// 内容构成权重
	weights := struct {
		code float64 // 代码和工具结果
		json float64 // JSON 参数
		text float64 // 中英文混合文本
	}{
		code: 0.587,
		json: 0.289,
		text: 0.124,
	}

	// 不同内容类型的 bytes/token 比率
	rates := struct {
		code float64 // 代码: 3.5
		json float64 // JSON: 3.0
		text float64 // 文本: 3.0
	}{
		code: 3.5,
		json: 3.0,
		text: 3.0,
	}

	// 计算加权平均
	optimalRate := weights.code*rates.code + weights.json*rates.json + weights.text*rates.text

	// 验证计算结果
	expectedRate := 3.3 // 理论计算值
	if optimalRate < expectedRate-0.1 || optimalRate > expectedRate+0.1 {
		t.Errorf("最优比率计算错误: 得到 %.1f, 期望 %.1f", optimalRate, expectedRate)
	}

	t.Logf("理论计算的最优 bytes/token 比率: %.1f", optimalRate)
}

// TestCurrentVsOptimalRate 比较当前保守估计与最优估计
func TestCurrentVsOptimalRate(t *testing.T) {
	currentRate := 3.0 // 当前保守估计
	optimalRate := 3.3 // 理论最优值

	// 测试不同上下文大小
	testSizes := []int{
		50000,   // 小型上下文
		150000,  // 中型上下文
		350000,  // 大型上下文 (平均值)
		1000000, // 超大上下文
	}

	for _, size := range testSizes {
		t.Run(string(rune(size)), func(t *testing.T) {
			currentTokens := int(float64(size) / currentRate)
			optimalTokens := int(float64(size) / optimalRate)
			diff := currentTokens - optimalTokens
			percent := float64(diff) / float64(currentTokens) * 100

			t.Logf("上下文 %d bytes:", size)
			t.Logf("  当前估计 (%.1f): %d tokens", currentRate, currentTokens)
			t.Logf("  最优估计 (%.1f): %d tokens", optimalRate, optimalTokens)
			t.Logf("  差异: %d tokens (%.1f%% 高估)", diff, percent)
		})
	}
}
