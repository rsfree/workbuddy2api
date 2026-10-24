package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 真实异步状态机回归：请求中禁止切换、关闭和重复提交；提交返回的任务 ID
// 必须保存，轮询期间也不能再发另一个提交。
func TestAppJSModelDebugBusyState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model debug state test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdDebugTemplate');
const end = src.indexOf("$('mdBody').addEventListener");
if (start < 0 || end < 0) throw new Error('debug state region not found');
const sinks = {};
const mk = id => (sinks[id] = { value: '', textContent: '', innerHTML: '', hidden: true, disabled: false, scrollIntoView() {} });
const toasts = [];
let fetchCalls = 0;
let resolveFetch;
const response = body => ({ status: 200, headers: { get: () => 'req-test' }, text: async () => JSON.stringify(body) });
const ctx = {
  JSON, String, Array, Object, Date, encodeURIComponent,
  mdAll: [{ id: 'cn:minimax-video-h3', type: 'video' }],
  mdDebug: null, mdDebugSeq: 0, mdVideoTaskID: '', mdDebugBusy: false, LS_KEY: 'wb2api.key',
  $: id => sinks[id] || mk(id), esc: s => String(s),
  toast: m => toasts.push(m), openKey() {},
  localStorage: { getItem: () => null }, window: { confirm: () => true },
  fetch: async () => { fetchCalls++; return new Promise(resolve => { resolveFetch = () => resolve(response(fetchCalls === 1 ? { id: 'task-1' } : { id: 'task-1', status: 'queued' })); }); },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.open=openModelDebug; this.close=closeModelDebug; this.send=sendModelDebug; this.state=() => ({ debug: mdDebug, busy: mdDebugBusy, task: mdVideoTaskID });', ctx);
(async () => {
  ctx.open('cn:minimax-video-h3');
  const openedState = ctx.state();
  const submitting = ctx.send(false);
  const sendingState = ctx.state();
  const busyBlocked = ctx.state().busy && sinks['mdDebugSend'].disabled && sinks['mdDebugPoll'].disabled;
  ctx.open('cn:minimax-video-h3');
  ctx.close();
  await ctx.send(false); // busy，必须直接返回，不增加 fetchCalls
  const duplicateBlocked = fetchCalls === 1 && toasts.length === 2 && sinks['mdDebugBox'].hidden === false;
  if (typeof resolveFetch !== 'function') throw new Error(JSON.stringify({ fetchCalls, toasts, openedState, sendingState, state: ctx.state(), payload: sinks['mdDebugPayload'] && sinks['mdDebugPayload'].value }));
  resolveFetch();
  await submitting;
  const submitted = !ctx.state().busy && ctx.state().task === 'task-1' && !sinks['mdDebugSend'].disabled && !sinks['mdDebugPoll'].disabled;
  const polling = ctx.send(true);
  await ctx.send(false); // 轮询中禁止另一次提交
  const pollBlocked = fetchCalls === 2 && ctx.state().busy;
  resolveFetch();
  await polling;
  process.stdout.write(JSON.stringify({
    busyBlocked, duplicateBlocked, submitted, pollBlocked,
    finished: !ctx.state().busy && fetchCalls === 2,
  }));
})().catch(e => { console.error(e); process.exit(1); });`
	f, err := os.CreateTemp(t.TempDir(), "model-debug-state-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model debug state node test failed: %v\n%s", err, out)
	}
	const want = `{"busyBlocked":true,"duplicateBlocked":true,"submitted":true,"pollBlocked":true,"finished":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model debug state=%s\nwant %s", out, want)
	}
}
