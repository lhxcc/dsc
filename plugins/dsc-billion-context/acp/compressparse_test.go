package acp

import "testing"

// compress 参数宽松解析回归：模型输出常带代码围栏、尾逗号或被截断，
// 严格 JSON 解析失败会让整轮压缩作废（DESIGN §6 明确要求 lenient）。
func TestParseCompressArgsLenient(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "正常对象",
			in:   `{"content":[{"startId":"m00001","endId":"m00005","summary":"做了什么"}]}`,
			want: 1,
		},
		{
			name: "代码围栏包裹",
			in:   "```json\n{\"content\":[{\"startId\":\"m00001\",\"endId\":\"m00005\",\"summary\":\"s\"}]}\n```",
			want: 1,
		},
		{
			name: "尾逗号",
			in:   `{"content":[{"startId":"m00001","endId":"m00005","summary":"s",},],}`,
			want: 1,
		},
		{
			name: "字段变体 startRef/endRef",
			in:   `{"content":[{"startRef":"m00001","endRef":"m00005","summary":"s"}]}`,
			want: 1,
		},
		{
			name: "前置说明文字",
			in:   `好的，压缩这一段：{"content":[{"startId":"m00001","endId":"m00005","summary":"s"}]}`,
			want: 1,
		},
		{
			name: "输出截断只回收完整区间",
			in:   `{"content":[{"startId":"m00001","endId":"m00005","summary":"first"},{"startId":"m00006","endId":"m00`,
			want: 1,
		},
		{
			name: "缺少 summary 的区间被丢弃",
			in:   `{"content":[{"startId":"m00001","endId":"m00005"},{"startId":"m00006","endId":"m00009","summary":"s"}]}`,
			want: 1,
		},
		{
			name: "空 content",
			in:   `{"content":[]}`,
			want: 0,
		},
		{
			name: "非 JSON",
			in:   `not json at all`,
			want: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseCompressArgs(c.in)
			if len(got) != c.want {
				t.Fatalf("范围数 = %d，want %d（输入 %q）", len(got), c.want, c.in)
			}
		})
	}
}

// TestParseCompressArgsSalvagesValues 截断救援时保留完整区间的字段值。
func TestParseCompressArgsSalvagesValues(t *testing.T) {
	in := `{"content":[{"startId":"m00002","endId":"m00007","summary":"第一段摘要","topic":"build"},{"startId":"m00008`
	got := ParseCompressArgs(in)
	if len(got) != 1 {
		t.Fatalf("应回收 1 个完整区间，got %d", len(got))
	}
	r := got[0]
	if r.StartRef != "m00002" || r.EndRef != "m00007" || r.Summary != "第一段摘要" || r.Topic != "build" {
		t.Fatalf("字段解析异常: %+v", r)
	}
}
