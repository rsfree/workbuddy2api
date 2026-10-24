package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 每种目录条目必须生成其真实端点的请求；视频去掉路由域前缀，聊天保留。
// cURL 使用环境变量占位符，不能把面板密钥写进复制内容或 shell 历史。
func TestAppJSModelDebugTemplatesAndCurl(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model debug test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const ts = src.indexOf('function mdDebugTemplate');
const te = src.indexOf('function openModelDebug');
const cs = src.indexOf('function mdCurlCommand');
const ce = src.indexOf('function mdMediaHtml');
if ([ts, te, cs, ce].some(v => v < 0)) throw new Error('debug helpers not found');
const ctx = { JSON, String, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(ts, te) + src.slice(cs, ce) +
  '\nthis.tpl=mdDebugTemplate; this.curl=mdCurlCommand;', ctx);
const chat = ctx.tpl({ id: 'global:kimi-k3-1', type: 'chat' });
const image = ctx.tpl({ id: 'cn:hunyuan-image-alpha', type: 'image' });
const edit = ctx.tpl({ id: 'cn:hunyuan-image-alpha-edit', type: 'image', tags: ['image-to-image'], edits_endpoint: '/v1/images/edits' });
const video = ctx.tpl({ id: 'cn:minimax-video-h3', type: 'video' });
const search = ctx.tpl({ id: 'workbuddy:web-search', type: 'search' });
const fetchTool = ctx.tpl({ id: 'workbuddy:web-fetch', type: 'fetch' });
const curl = ctx.curl(chat, { prompt: "it's" }, 'https://gw.example');
process.stdout.write(JSON.stringify({
  chat: chat.endpoint === '/v1/chat/completions' && chat.payload.model === 'global:kimi-k3-1' && chat.payload.stream === false,
  image: image.endpoint === '/v1/images/generations' && image.payload.n === 1,
  edit: edit.endpoint === '/v1/images/edits' && edit.payload.image.length === 1,
  video: video.endpoint === '/v1/videos/generations' && video.payload.model === 'minimax-video-h3' && video.payload.seconds <= 10,
  search: search.endpoint === '/v1/search/messages' && search.payload.tools[0].name === 'web_search',
  fetch: fetchTool.endpoint === '/v1/fetch',
  keyPlaceholder: curl.includes('${WB2API_KEY}'),
  shellEscaped: curl.includes("it'\"'\"'s"),
  noHardcodedBearer: !curl.includes('Bearer sk-'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-debug-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model debug node test failed: %v\n%s", err, out)
	}
	const want = `{"chat":true,"image":true,"edit":true,"video":true,"search":true,"fetch":true,"keyPlaceholder":true,"shellEscaped":true,"noHardcodedBearer":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model debug=%s\nwant %s", out, want)
	}
}
