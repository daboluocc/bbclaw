package extract

import "testing"

func TestNormalizeReply(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "plain CJK reply unchanged",
			in:   "你好你好,我在呢!",
			want: "你好你好,我在呢!",
		},
		{
			name: "token-count chrome glued to reply tail is stripped",
			in:   "谢谢周老板夸奖,随时听候吩咐。  ↓ 3 tokens)",
			want: "谢谢周老板夸奖,随时听候吩咐。",
		},
		{
			name: "reply legitimately mentioning tokens is left intact",
			in:   "你的余额还有 100 tokens 可用",
			want: "你的余额还有 100 tokens 可用",
		},
		{
			name: "CJK word split by a hard wrap is rejoined with no space",
			in:   "工作目\n录现在是 bbclaw 的 adapter_v2。",
			want: "工作目录现在是 bbclaw 的 adapter_v2。",
		},
		{
			name: "Latin sentence wrapped across rows rejoins with a space",
			in:   "The capital of France is Paris.\nIt has been the country's capital\nand is its largest city.",
			want: "The capital of France is Paris. It has been the country's capital and is its largest city.",
		},
		{
			name: "runs of padding spaces collapse to one",
			in:   "我是   Claude Code,Anthropic 推出的命令行       AI",
			want: "我是 Claude Code,Anthropic 推出的命令行 AI",
		},
		{
			name: "2-space continuation indent is stripped",
			in:   "你好!\n  我是助手。",
			want: "你好!我是助手。",
		},
		{
			name: "blank line is a real paragraph break (kept as one newline)",
			in:   "你好你好,我在呢!\n\n对了,咱们还没正式认识——我该怎么称呼你?",
			want: "你好你好,我在呢!\n对了,咱们还没正式认识——我该怎么称呼你?",
		},
		{
			name: "CJK-then-Latin wrap rejoins with no space",
			in:   "推出的命令行 AI\n编程助手,很高兴帮你。",
			want: "推出的命令行 AI编程助手,很高兴帮你。",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeReply(tc.in); got != tc.want {
				t.Errorf("NormalizeReply(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}
