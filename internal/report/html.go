package report

import (
	"encoding/json"
	"strings"

	"cfdns/internal/state"
)

// Generate 由 State 生成单文件、自包含的 HTML 报告。
// 顶部为域名 tab 导航, 点击后内容从上到下平铺; 无需任何外部依赖。
// 长文本(内容/备注)超出列宽时截断显示, hover 单元格时该行出现复制图标, 可一键复制全文。
func Generate(s state.State) string {
	b, _ := json.Marshal(s)
	tmpl := `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CFDNS 域名解析报告</title>
<style>
:root{
  --bg:#f5f6f8; --card:#ffffff; --border:#e4e7eb; --text:#1f2329; --muted:#8a9099;
  --orange:#f38020; --grey:#9aa0a6; --bar:#1f2329;
}
*{box-sizing:border-box}
body{margin:0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;background:var(--bg);color:var(--text);font-size:14px}
header{padding:16px 20px;border-bottom:1px solid var(--border);background:var(--card)}
h1{font-size:18px;margin:0}
.meta{color:var(--muted);font-size:12px;margin-top:4px}
nav{display:flex;flex-wrap:wrap;gap:8px;padding:12px 20px;background:var(--card);border-bottom:1px solid var(--border);position:sticky;top:0;z-index:5}
.tab{padding:6px 14px;border:1px solid var(--border);border-radius:999px;cursor:pointer;background:#fff;color:var(--text);font-size:13px;user-select:none}
.tab:hover{border-color:#c4c9d0}
.tab.active{background:var(--bar);color:#fff;border-color:var(--bar)}
.panel{display:none;padding:20px}
.panel.active{display:block}
.zonehead{display:flex;align-items:center;gap:12px;margin-bottom:6px;flex-wrap:wrap}
.badge{padding:3px 10px;border-radius:999px;font-size:12px;font-weight:600}
.badge.ssl{background:#eaf0ff;color:#2b5ce6;border:1px solid #c9d8ff}
.sub{color:var(--muted);font-size:12px}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:8px;overflow:hidden;margin-top:12px;table-layout:fixed}
th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--border);font-size:13px;vertical-align:middle;overflow:hidden}
th{background:#fafbfc;color:var(--muted);font-weight:600}
col.c-idx{width:44px}
col.c-name{width:20%}
col.c-type{width:62px}
col.c-proxy{width:76px}
col.c-ttl{width:54px}
col.c-cmt{width:16%}
/* 内容/备注列: 内部 .trunc 占满剩余宽度并截断, 复制图标在 hover 时出现 */
.cw{display:flex;align-items:center;gap:6px;min-width:0}
.trunc{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.copy-btn{flex:0 0 auto;display:none;cursor:pointer;color:var(--muted);align-items:center;justify-content:center;width:18px;height:18px;border-radius:4px}
.copy-btn:hover{color:var(--bar);background:#eef0f3}
.cw:hover .copy-btn{display:inline-flex}
.copy-btn.copied{color:#2e9b4e}
.pill{display:inline-block;padding:3px 12px;border-radius:999px;font-size:12px;font-weight:600;color:#fff}
.pill.proxy{background:var(--orange)}
.pill.dns{background:var(--grey)}
.empty{color:var(--muted);padding:20px}
.idx{color:var(--muted);font-variant-numeric:tabular-nums}
.perhost{margin-top:8px;display:flex;flex-wrap:wrap;gap:6px;align-items:center}
.perhost .lbl{color:var(--muted);font-size:12px}
</style>
</head>
<body>
<header>
  <h1>CFDNS 域名解析报告</h1>
  <div class="meta" id="meta"></div>
</header>
<nav id="nav"></nav>
<div id="panels"></div>
<script>
var STATE = __STATE__;
var COPY_SVG='<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
var nav=document.getElementById('nav');
var panels=document.getElementById('panels');
document.getElementById('meta').textContent='生成时间：'+ (STATE.generated_at||'') +'　域名数：'+ ((STATE.zones||[]).length);
(STATE.zones||[]).forEach(function(z,i){
  var t=document.createElement('div');
  t.className='tab'+(i===0?' active':'');
  t.textContent=z.domain;
  t.onclick=function(){
    var ts=document.querySelectorAll('.tab'); for(var k=0;k<ts.length;k++) ts[k].classList.remove('active');
    t.classList.add('active');
    var ps=document.querySelectorAll('.panel'); for(var k=0;k<ps.length;k++) ps[k].classList.remove('active');
    document.getElementById('p'+i).classList.add('active');
  };
  nav.appendChild(t);

  var p=document.createElement('div');
  p.className='panel'+(i===0?' active':'');
  p.id='p'+i;
  var ssl=z.ssl||'-';
  var html='<div class="zonehead"><span class="badge ssl">SSL: '+esc(ssl)+'</span><span class="sub">zone_id: '+esc(z.zone_id||'')+'</span></div>';
  if(z.per_host_ssl && Object.keys(z.per_host_ssl).length){
    html+='<div class="perhost"><span class="lbl">按主机SSL:</span>';
    Object.keys(z.per_host_ssl).forEach(function(h){
      html+='<span class="badge ssl">'+esc(h)+' → '+esc(z.per_host_ssl[h])+'</span>';
    });
    html+='</div>';
  }
  if(!z.records||!z.records.length){
    html+='<div class="empty">该域名下无 DNS 记录</div>';
  } else {
    html+='<table><colgroup><col class="c-idx"><col class="c-name"><col class="c-type"><col class="c-content"><col class="c-proxy"><col class="c-ttl"><col class="c-cmt"></colgroup><thead><tr><th>#</th><th>名称</th><th>类型</th><th>内容</th><th>代理</th><th>TTL</th><th>备注</th></tr></thead><tbody>';
    z.records.forEach(function(r,idx){
      var cls=r.proxied?'proxy':'dns';
      var label=r.proxied?'Proxy':'DNS';
      var content=esc(r.content);
      var cmt=esc(r.comment||'');
      var cmtBtn = cmt ? '<span class="copy-btn" title="复制" data-full="'+cmt+'">'+COPY_SVG+'</span>' : '';
      html+='<tr><td class="idx">'+(idx+1)+'</td>'
        +'<td>'+esc(r.name)+'</td>'
        +'<td>'+esc(r.type)+'</td>'
        +'<td class="c-content"><div class="cw"><span class="trunc" title="'+content+'">'+content+'</span><span class="copy-btn" title="复制" data-full="'+content+'">'+COPY_SVG+'</span></div></td>'
        +'<td><span class="pill '+cls+'">'+label+'</span></td>'
        +'<td>'+(r.ttl||'')+'</td>'
        +'<td class="c-cmt"><div class="cw"><span class="trunc" title="'+cmt+'">'+cmt+'</span>'+cmtBtn+'</div></td>'
        +'</tr>';
    });
    html+='</tbody></table>';
  }
  p.innerHTML=html;
  panels.appendChild(p);
});
// 复制图标: 点击把 data-full 全文写入剪贴板, 并短暂反馈
document.addEventListener('click', function(e){
  var btn = e.target && e.target.closest ? e.target.closest('.copy-btn') : null;
  if(!btn) return;
  var txt = btn.getAttribute('data-full') || '';
  if(navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(txt).then(function(){
      btn.classList.add('copied');
      var prev = btn.innerHTML;
      btn.innerHTML = '✓';
      setTimeout(function(){ btn.classList.remove('copied'); btn.innerHTML = prev; }, 1200);
    }).catch(function(){});
  }
});
function esc(s){return (''+ (s==null?'':s)).replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
</script>
</body>
</html>`
	return strings.Replace(tmpl, "__STATE__", string(b), 1)
}
