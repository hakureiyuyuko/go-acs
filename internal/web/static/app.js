// 设备详情页/概览页上的几处小交互，纯原生 JS，没有依赖。
//
//   1. 表格分页：带 data-pager="20" 的表格默认 20 条/页
//   2. 参数名过滤：过滤之后重新分页（两者是配合关系，不是各干各的）
//   3. 概览页搜索框：防抖自动提交（服务端过滤）
//   4. 破坏性操作（重启设备等）的二次确认
document.addEventListener("DOMContentLoaded", function () {
  var pagers = setupPagers(document);
  wireParamFilter(pagers);
  wireSearchBox();
  wireThemeToggle();
  wireConfirms();
});

// ---------- 破坏性操作的二次确认 ----------
//
// 要确认的文字写在 form 的 data-confirm 属性上，不是拼在 JS 里：
// 属性由模板负责转义（设备名里带引号也不会把脚本搞坏），这里只负责弹框。
function wireConfirms() {
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (!window.confirm(form.getAttribute("data-confirm"))) {
        e.preventDefault();
      }
    });
  });
}

// ---------- 日间 / 夜间模式 ----------
//
// 主题值在页面 <head> 的肉联脚本里已经写好（避免刷新闪一下），
// 这里只负责按钮的标签与切换。
function wireThemeToggle() {
  var btn = document.getElementById("theme-toggle");
  var root = document.documentElement;
  if (!btn) return;

  function label() {
    btn.textContent = root.getAttribute("data-theme") === "light"
      ? "🌙 切换到夜间" : "☀ 切换到日间";
  }
  label();

  btn.addEventListener("click", function () {
    var next = root.getAttribute("data-theme") === "light" ? "dark" : "light";
    root.setAttribute("data-theme", next);
    try {
      localStorage.setItem("theme", next);
    } catch (e) { /* 隐私模式下写不进去，忽略 */ }
    label();
  });
}

// ---------- 表格分页 ----------
//
// 数据本来就全部渲染在 DOM 里了，所以在浏览器端分页就够 ——
// 不用往返服务端，而且能和「参数过滤」天然配合：过滤出子集后重新分页。

function setupPagers(root) {
  var pagers = {};
  Array.prototype.forEach.call(root.querySelectorAll("table[data-pager]"), function (table) {
    var size = parseInt(table.getAttribute("data-pager"), 10);
    if (!size || size < 1) size = 20;
    var pager = makePager(table, size);
    if (table.id) pagers[table.id] = pager;
  });
  return pagers;
}

function makePager(table, defaultSize) {
  var tbody = table.tBodies[0];
  if (!tbody) return null;
  var rows = Array.prototype.slice.call(tbody.rows);

  var size = defaultSize;
  var page = 1;
  var predicate = function () { return true; };

  // 行数本来就不到一页时，分页条纯属噪音，直接不显示。
  if (rows.length <= defaultSize) {
    return { setFilter: function () {}, render: function () {} };
  }

  var bar = document.createElement("div");
  bar.className = "pager";

  var prev = mkBtn("‹ 上一页");
  var next = mkBtn("下一页 ›");
  var info = document.createElement("span");
  info.className = "pager-info";

  var sizeSel = document.createElement("select");
  sizeSel.className = "pager-size";
  [20, 50, 100, 0].forEach(function (n) {
    var o = document.createElement("option");
    o.value = String(n);
    o.textContent = n === 0 ? "全部" : n + " 条/页";
    sizeSel.appendChild(o);
  });
  sizeSel.value = String(defaultSize);

  bar.appendChild(prev);
  bar.appendChild(info);
  bar.appendChild(next);
  bar.appendChild(sizeSel);
  table.parentNode.insertBefore(bar, table.nextSibling);

  function mkBtn(text) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "ghost";
    b.textContent = text;
    return b;
  }

  function render() {
    var list = rows.filter(predicate);
    var pages = size > 0 ? Math.max(1, Math.ceil(list.length / size)) : 1;
    if (page > pages) page = pages;
    if (page < 1) page = 1;

    var startIdx = size > 0 ? (page - 1) * size : 0;
    var endIdx = size > 0 ? Math.min(startIdx + size, list.length) : list.length;

    // 先全部藏起来，再只显示本页的 —— 比逐行切换简单且不会漏
    for (var i = 0; i < rows.length; i++) rows[i].style.display = "none";
    for (var j = startIdx; j < endIdx; j++) list[j].style.display = "";

    info.textContent = size > 0
      ? "共 " + list.length + " 条 · 第 " + page + " / " + pages + " 页"
      : "共 " + list.length + " 条";
    prev.disabled = page <= 1;
    next.disabled = page >= pages;
  }

  prev.addEventListener("click", function () { page--; render(); });
  next.addEventListener("click", function () { page++; render(); });
  sizeSel.addEventListener("change", function () {
    size = parseInt(sizeSel.value, 10);
    page = 1;
    render();
  });

  render();

  return {
    // setFilter 会重置到第 1 页 —— 过滤后还停在第 47 页会很困惑
    setFilter: function (fn) { predicate = fn; page = 1; render(); },
    render: render,
  };
}

// ---------- 参数名过滤 ----------

function wireParamFilter(pagers) {
  var box = document.getElementById("param-filter");
  if (!box) return;
  var pager = pagers["param-table"];

  function apply() {
    var q = box.value.trim().toLowerCase();
    var fn = function (tr) {
      var name = tr.getAttribute("data-name") || "";
      return !q || name.toLowerCase().indexOf(q) >= 0;
    };
    if (pager) {
      pager.setFilter(fn);
    } else {
      // 兜底：没有分页条时就直接显隐
      Array.prototype.forEach.call(
        document.querySelectorAll("#param-table tbody tr"),
        function (tr) { tr.style.display = fn(tr) ? "" : "none"; }
      );
    }
  }

  box.addEventListener("input", apply);
  apply();
}

// ---------- 概览页搜索框 ----------
// 输入后停顿一下自动提交（服务端过滤，结果 URL 可分享）。
// 刷新后把光标放回输入框，让连续输入不被打断。
function wireSearchBox() {
  var box = document.getElementById("dev-search");
  if (!box || !box.form) return;

  if (box.value) {
    box.focus();
    try {
      box.setSelectionRange(box.value.length, box.value.length);
    } catch (e) { /* 某些浏览器对非 text 类型不支持，忽略 */ }
  }

  var timer = null;
  box.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { box.form.submit(); }, 350);
  });
}
