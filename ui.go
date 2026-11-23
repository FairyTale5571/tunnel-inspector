package main

// Весь UI рендерится через textContent: тела запросов приходят из интернета,
// это недоверенные данные, и ни один байт из них не попадает в innerHTML.
const indexHTML = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Tunnel Inspector</title>
<style>
:root{--bg:#14161a;--panel:#1b1e24;--line:#2b303a;--fg:#e6e8ec;--dim:#8d95a3;
--acc:#6ea8fe;--ok:#59c279;--warn:#e0b65a;--err:#e4706b;
--mono:ui-monospace,SFMono-Regular,Menlo,monospace}
*{box-sizing:border-box}
body{margin:0;height:100vh;display:flex;flex-direction:column;background:var(--bg);
color:var(--fg);font:13px/1.5 -apple-system,system-ui,sans-serif}
header{display:flex;gap:12px;align-items:center;padding:8px 12px;
border-bottom:1px solid var(--line);flex:0 0 auto}
header b{font-size:13px;font-weight:600}
header .sp{flex:1}
button{background:var(--panel);color:var(--fg);border:1px solid var(--line);
border-radius:6px;padding:5px 11px;font:inherit;cursor:pointer}
button:hover{border-color:var(--acc)}
button.primary{background:var(--acc);color:#0d1017;border-color:var(--acc);font-weight:600}
#stat{color:var(--dim);font-size:12px}
main{flex:1;display:flex;min-height:0}
#list{width:38%;min-width:300px;max-width:560px;overflow-y:auto;
border-right:1px solid var(--line)}
#detail{flex:1;overflow-y:auto;padding:14px 16px}
.row{display:grid;grid-template-columns:52px 1fr auto;gap:8px;align-items:baseline;
padding:7px 12px;border-bottom:1px solid var(--line);cursor:pointer}
.row:hover{background:#1e2228}
.row.sel{background:#232833}
.row .m{font:600 11px var(--mono);color:var(--dim)}
.row .u{font:12px var(--mono);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.row .s{font:600 11px var(--mono)}
.row .meta{grid-column:2/4;color:var(--dim);font-size:11px}
.s2{color:var(--ok)}.s3{color:var(--acc)}.s4{color:var(--warn)}.s5{color:var(--err)}
.tag{font-size:10px;padding:0 5px;border-radius:4px;background:#30374a;color:var(--acc);
margin-left:6px;vertical-align:1px}
h2{font-size:12px;text-transform:uppercase;letter-spacing:.06em;color:var(--dim);
margin:18px 0 7px}
h2:first-child{margin-top:0}
pre{margin:0;padding:10px;background:var(--panel);border:1px solid var(--line);
border-radius:6px;font:12px/1.55 var(--mono);white-space:pre-wrap;word-break:break-word;
max-height:40vh;overflow:auto}
table{width:100%;border-collapse:collapse;font:12px var(--mono)}
td{padding:2px 8px 2px 0;vertical-align:top;border-bottom:1px solid #22262e}
td.k{color:var(--dim);white-space:nowrap;width:1%}
.note{color:var(--warn);font-size:12px;margin:5px 0}
.empty{color:var(--dim);padding:40px 16px;text-align:center}
label{display:block;color:var(--dim);font-size:11px;text-transform:uppercase;
letter-spacing:.06em;margin:10px 0 4px}
input,textarea{width:100%;background:var(--panel);color:var(--fg);
border:1px solid var(--line);border-radius:6px;padding:7px 9px;font:12px var(--mono)}
textarea{resize:vertical}
.rrow{display:flex;gap:8px}
.rrow input:first-child{width:100px;flex:0 0 auto;text-transform:uppercase}
#replay{border-top:1px solid var(--line);margin-top:20px;padding-top:4px}
.tools{display:flex;gap:8px;align-items:center;margin-top:6px;flex-wrap:wrap}
.tools button{padding:3px 9px;font-size:12px}
.hint{color:var(--dim);font-size:11px}
#rp-body{line-height:1.5}
</style></head><body>
<header>
  <b>Tunnel Inspector</b>
  <span id="stat"></span>
  <span class="sp"></span>
  <button id="pause">Пауза</button>
  <button id="clear">Очистить</button>
</header>
<main>
  <div id="list"><div class="empty">Ждём запросов…</div></div>
  <div id="detail"><div class="empty">Выбери запрос слева</div></div>
</main>
<script>
"use strict";
var listEl=document.getElementById("list"),detEl=document.getElementById("detail"),
    statEl=document.getElementById("stat");
var items=[],lastId=0,selected=null,paused=false,empty=true;

function el(tag,cls,text){var n=document.createElement(tag);
  if(cls)n.className=cls; if(text!==undefined)n.textContent=text; return n;}
function cls(st){return st>=500?"s5":st>=400?"s4":st>=300?"s3":st>=200?"s2":"";}
function kb(n){return n<0?"? Б":n<1024?n+" Б":n<1048576?(n/1024).toFixed(1)+" КБ"
  :(n/1048576).toFixed(2)+" МБ";}

function rowNode(it){
  var r=el("div","row");r.dataset.id=it.id;
  r.appendChild(el("span","m",it.method));
  var u=el("span","u",it.url);
  if(it.replay){var t=el("span","tag","replay");u.appendChild(t);}
  r.appendChild(u);
  r.appendChild(el("span","s "+cls(it.status),it.err?"ERR":String(it.status||"-")));
  var meta=el("div","meta",it.time+" · "+it.durationMs+" мс · "+kb(it.respSize)
    +(it.err?" · "+it.err:""));
  r.appendChild(meta);
  r.addEventListener("click",function(){select(it.id);});
  return r;
}

function select(id){
  selected=id;
  Array.prototype.forEach.call(listEl.children,function(c){
    if(c.dataset)c.classList.toggle("sel",c.dataset.id===String(id));});
  fetch("/api/entries/"+id).then(function(r){
    if(!r.ok)throw new Error("запись не найдена");return r.json();}).then(render)
    .catch(function(e){detEl.textContent="";detEl.appendChild(el("div","empty",e.message));});
}

function headerTable(pairs){
  var t=el("table");
  pairs.sort(function(a,b){return a[0]<b[0]?-1:1;}).forEach(function(p){
    var tr=el("tr");tr.appendChild(el("td","k",p[0]));
    tr.appendChild(el("td","",p[1]));t.appendChild(tr);});
  return t;
}

// reformatJSON переставляет ТОЛЬКО пробелы вне строк, а все литералы
// оставляет как есть. JSON.parse + JSON.stringify здесь не годятся: они
// теряют точность чисел длиннее 2^53 (а это telegram-овские id каналов)
// и превращают 1.0 в 1 — для тела, которое потом уйдёт обратно, это брак.
// Возвращает null, если это не цельный JSON (например, обрезанное тело).
function reformatJSON(src,pretty){
  var s=String(src||""),out="",depth=0,inStr=false,esc=false;
  var head=s.replace(/^[\s\uFEFF]+/,"")[0];
  if(head!=="{"&&head!=="[")return null;
  function pad(n){
    if(!pretty)return "";
    var t="\n";for(var k=0;k<n;k++)t+="  ";return t;
  }
  for(var i=0;i<s.length;i++){
    var c=s[i];
    if(inStr){
      out+=c;
      if(esc)esc=false;
      else if(c==="\\")esc=true;
      else if(c==='"')inStr=false;
      continue;
    }
    if(c==='"'){inStr=true;out+=c;continue;}
    if(c===" "||c==="\t"||c==="\n"||c==="\r")continue;
    if(c==="{"||c==="["){
      out+=c;depth++;
      var j=i+1;while(j<s.length&&/\s/.test(s[j]))j++;
      if(s[j]!=="}"&&s[j]!=="]")out+=pad(depth);
      continue;
    }
    if(c==="}"||c==="]"){
      depth--;
      var prev=out.charAt(out.length-1);
      if(prev!=="{"&&prev!=="[")out+=pad(depth);
      out+=c;continue;
    }
    if(c===","){out+=c+pad(depth);continue;}
    if(c===":"){out+=pretty?": ":":";continue;}
    out+=c;
  }
  if(inStr||depth!==0)return null;
  return out;
}

function bodyBlock(b){
  var box=document.createDocumentFragment();
  if(b.note)box.appendChild(el("div","note",b.note));
  if(b.binary){box.appendChild(el("pre","",
    "("+kb(b.size)+", не показывается)"));return box;}
  if(!b.text){box.appendChild(el("pre","","(пусто)"));return box;}
  box.appendChild(el("pre","",reformatJSON(b.text,true)||b.text));
  if(b.trunc)box.appendChild(el("div","note","показаны первые 32 КБ"+
    (b.size>=0?" из "+kb(b.size):"")));
  return box;
}

function render(d){
  detEl.textContent="";
  var h=el("div","");
  h.appendChild(el("h2","","Запрос"));
  h.appendChild(el("pre","",d.method+" "+d.url+"\n"+
    (d.remoteAddr?"client: "+d.remoteAddr+"\n":"")+
    "status: "+(d.err?d.err:d.status)+" · "+d.durationMs+" мс · "+d.time));
  detEl.appendChild(h);
  detEl.appendChild(el("h2","","Заголовки запроса"));
  detEl.appendChild(headerTable(d.reqHeaders||[]));
  detEl.appendChild(el("h2","","Тело запроса"));
  detEl.appendChild(bodyBlock(d.req));
  detEl.appendChild(el("h2","","Заголовки ответа"));
  detEl.appendChild(headerTable(d.respHeaders||[]));
  detEl.appendChild(el("h2","","Тело ответа"));
  detEl.appendChild(bodyBlock(d.resp));
  detEl.appendChild(replayForm(d));
}

function replayForm(d){
  var w=el("div","");w.id="replay";
  w.appendChild(el("h2","","Изменить и переиграть"));
  var row=el("div","rrow");
  var m=el("input");m.value=d.method;m.id="rp-method";
  m.setAttribute("aria-label","Метод");
  var u=el("input");u.value=d.url;u.id="rp-url";
  u.setAttribute("aria-label","Путь");
  row.appendChild(m);row.appendChild(u);w.appendChild(row);
  // label без for невидим для дерева доступности и скринридеров,
  // поэтому связываем каждую подпись с полем явно.
  var lh=el("label","","Заголовки (по строке на каждый)");
  lh.htmlFor="rp-headers";w.appendChild(lh);
  var hh=el("textarea");hh.rows=8;hh.id="rp-headers";
  hh.value=(d.reqHeaders||[]).map(function(p){return p[0]+": "+p[1];}).join("\n");
  w.appendChild(hh);
  var lb=el("label","","Тело");lb.htmlFor="rp-body";w.appendChild(lb);
  var raw=d.req.binary?"":d.req.text||"";
  var bb=el("textarea");bb.id="rp-body";
  var formatted=reformatJSON(raw,true);
  bb.value=formatted||raw;
  bb.rows=formatted?Math.min(26,Math.max(8,formatted.split("\n").length+1)):6;
  if(d.req.binary||d.req.trunc)
    w.appendChild(el("div","note","исходное тело недоступно целиком — "+
      "отправится то, что в поле"));
  w.appendChild(bb);
  if(formatted){
    var tools=el("div","tools");
    var fmt=el("button","","{ } Формат"),min=el("button","","Сжать в строку");
    fmt.title="Расставить отступы";min.title="Убрать переносы и отступы";
    fmt.addEventListener("click",function(){
      var r=reformatJSON(bb.value,true);
      if(r===null){fmt.textContent="не JSON";setTimeout(function(){
        fmt.textContent="{ } Формат";},1200);return;}
      bb.value=r;bb.rows=Math.min(26,Math.max(8,r.split("\n").length+1));});
    min.addEventListener("click",function(){
      var r=reformatJSON(bb.value,false);
      if(r===null){min.textContent="не JSON";setTimeout(function(){
        min.textContent="Сжать в строку";},1200);return;}
      bb.value=r;bb.rows=6;});
    tools.appendChild(fmt);tools.appendChild(min);
    // Отправляется ровно то, что в поле. Для подавляющего большинства
    // приложений пробелы в JSON ничего не значат, но подпись тела
    // (Stripe, GitHub) считается по сырым байтам — там нужно «Сжать».
    tools.appendChild(el("span","hint",
      "уходит ровно то, что в поле — если получатель проверяет подпись тела, сожми обратно"));
    w.appendChild(tools);
  }
  var send=el("button","primary","Отправить");
  var out=el("span","note","");out.style.marginLeft="10px";
  send.addEventListener("click",function(){
    send.disabled=true;out.textContent="отправляю…";
    fetch("/api/replay",{method:"POST",headers:{"Content-Type":"application/json"},
      body:JSON.stringify({method:m.value,url:u.value,headers:hh.value,body:bb.value})})
      .then(function(r){return r.json();})
      .then(function(j){
        out.textContent=j.error?("ошибка: "+j.error):("отправлено → "+j.status);
        send.disabled=false;poll(true);if(j.id)select(j.id);})
      .catch(function(e){out.textContent="ошибка: "+e.message;send.disabled=false;});
  });
  var bar=el("div","");bar.style.marginTop="12px";
  bar.appendChild(send);bar.appendChild(out);w.appendChild(bar);
  return w;
}

function poll(force){
  if(paused&&!force)return;
  fetch("/api/entries?since="+lastId).then(function(r){return r.json();})
  .then(function(fresh){
    if(!fresh||!fresh.length)return;
    if(empty){listEl.textContent="";empty=false;}
    // /api/entries отдаёт новые первыми; вставляем с конца, иначе
    // insertBefore(firstChild) переворачивает каждую порцию.
    fresh.slice().reverse().forEach(function(it){
      if(it.id>lastId)lastId=it.id;
      items.unshift(it);
      listEl.insertBefore(rowNode(it),listEl.firstChild);});
    while(listEl.children.length>100)listEl.removeChild(listEl.lastChild);
    items=items.slice(0,100);
    statEl.textContent=items.length+" записей";
  }).catch(function(){statEl.textContent="нет связи с инспектором";});
}

document.getElementById("pause").addEventListener("click",function(){
  paused=!paused;this.textContent=paused?"Продолжить":"Пауза";});
document.getElementById("clear").addEventListener("click",function(){
  fetch("/api/clear",{method:"POST"}).then(function(){
    items=[];lastId=0;selected=null;empty=true;
    listEl.textContent="";listEl.appendChild(el("div","empty","Ждём запросов…"));
    detEl.textContent="";detEl.appendChild(el("div","empty","Выбери запрос слева"));
    statEl.textContent="";});});

poll();setInterval(poll,1500);
</script></body></html>
`
