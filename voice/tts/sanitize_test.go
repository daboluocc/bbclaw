package tts

import "testing"

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "plain text untouched",
			in:   "你好，今天天气真好。",
			want: "你好，今天天气真好。",
		},
		{
			name: "bold and italic stripped",
			in:   "我使用的模型是 **Sonnet 4.5** (claude-sonnet-4-5) 真的 *很厉害*。",
			want: "我使用的模型是 Sonnet 4.5 (claude-sonnet-4-5) 真的 很厉害。",
		},
		{
			name: "inline code keeps content",
			in:   "我的工作目录是 `/Volumes/1TB/github/daboluocc-bbclaw/adapter`",
			want: "我的工作目录是 /Volumes/1TB/github/daboluocc-bbclaw/adapter",
		},
		{
			name: "fenced code block keeps inner",
			in:   "运行：\n```bash\nmake build\n```\n然后等待。",
			want: "运行： make build 然后等待。",
		},
		{
			name: "markdown link keeps text",
			in:   "详见 [文档](https://example.com/doc) 第三章。",
			want: "详见 文档 第三章。",
		},
		{
			name: "markdown image keeps alt",
			in:   "看这张图 ![架构图](http://x/a.png) 就明白了。",
			want: "看这张图 架构图 就明白了。",
		},
		{
			name: "headers stripped",
			in:   "## 标题\n正文内容",
			want: "标题 正文内容",
		},
		{
			name: "list bullets stripped",
			in:   "- 第一项\n- 第二项\n1. 编号项",
			want: "第一项 第二项 编号项",
		},
		{
			name: "blockquote stripped",
			in:   "> 引用一段\n> 第二行",
			want: "引用一段 第二行",
		},
		{
			name: "horizontal rule removed",
			in:   "上面\n---\n下面",
			want: "上面 下面",
		},
		{
			name: "strikethrough stripped",
			in:   "这是 ~~删除~~ 内容",
			want: "这是 删除 内容",
		},
		{
			name: "html tags removed",
			in:   "前面<br>后面<b>粗体</b>结束",
			want: "前面后面粗体结束",
		},
		{
			name: "zero-width and bom removed",
			in:   "\uFEFF你\u200b好\u200d世界",
			want: "你好世界",
		},
		{
			name: "control chars stripped but tab newline kept",
			in:   "第一行\n\t第二行\x07包含响铃",
			want: "第一行 第二行包含响铃",
		},
		{
			name: "multiple whitespace collapsed",
			in:   "多个    空格\n\n\n换行",
			want: "多个 空格 换行",
		},
		{
			name: "real-world model reply",
			in:   "\n\n我的工作目录是 `/Volumes/1TB/github/daboluocc-bbclaw/adapter`\n\n我使用的模型是 **Sonnet 4.5** (claude-sonnet-4-5)",
			want: "我的工作目录是 /Volumes/1TB/github/daboluocc-bbclaw/adapter 我使用的模型是 Sonnet 4.5 (claude-sonnet-4-5)",
		},
		{
			name: "nested formatting strips fully",
			in:   "**__hello__**",
			want: "hello",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in)
			if got != tc.want {
				t.Errorf("Sanitize(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
