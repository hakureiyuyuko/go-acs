// 面板前端脚本：分页、参数过滤、搜索框、主题、自动刷新、二次确认、弹窗。
//
// 文案统一走 t() 取译文：译文由服务端按当前语言写进 window.I18N
//（见 internal/web/web.go 的 pageCommon），查不到就原样用中文 —— 不会出现空白或键名。

function t(key) {
  try {
    if (window.I18N && window.I18N[key]) return window.I18N[key];
  } catch (e) { /* 忽略 */ }
  return key;
}
// 设备详情页/概览页上的几处小交互，纯原生 JS，没有依赖。
//
//   1. 表格分页：带 data-pager="20" 的表格默认 20 条/页
//   2. 参数名过滤：过滤之后重新分页（两者是配合关系，不是各干各的）
//   3. 概览页搜索框：防抖自动提交（服务端过滤）
//   4. 破坏性操作（重启设备等）的二次确认
//   5. 终端弹窗：data-modal 打开、点遮罩或 ✕ 或 Esc 关闭
//   6. 首页的 5 秒自动刷新开关（状态记在 localStorage）
//   7. 设备列表的多选：全选本页 + 已选台数 + 提交时只带「看得见」的那些
//   8. 批量页上的「全选 / 清空」与已选台数
document.addEventListener("DOMContentLoaded", function () {
  var pagers = setupPagers(document);
  wireParamFilter(pagers);
  wireSearchBox();
  wireThemeToggle();
  wireAutoRefresh();
  wireConfirms();
  wireModals();
  wireBatchSelection();
  wireBatchDevicePicker();
});

// ---------- 批量页：目标设备的全选 / 清空 / 计数 ----------
function wireBatchDevicePicker() {
  var boxes = Array.prototype.slice.call(document.querySelectorAll(".ids-check"));
  var count = document.getElementById("ids-count");
  var on = document.getElementById("ids-all-on");
  var off = document.getElementById("ids-all-off");
  if (boxes.length === 0) return;

  function refresh() {
    var n = boxes.filter(function (b) { return b.checked; }).length;
    if (count) count.textContent = t("已选 %d 台").replace("%d", n);
  }
  if (on) on.addEventListener("click", function () { boxes.forEach(function (b) { b.checked = true; }); refresh(); });
  if (off) off.addEventListener("click", function () { boxes.forEach(function (b) { b.checked = false; }); refresh(); });
  boxes.forEach(function (b) { b.addEventListener("change", refresh); });
  refresh();
}

// ---------- 设备列表多选（批量下发 WiFi 参数用）----------
//
// 复选框用 form="batch-form" 挂到工具栏那个表单上，所以**没有 JS 也能提交**；
// 这里补四件事：
//   1. 表头的全选框（只管当前页）；
//   2. 「已选 N 台」的实时计数（选了没反应最让人心里没底）；
//   3. 选择记在 sessionStorage —— 概览页可能开着 5 秒自动刷新，一刷新勾选就没了；
//      翻页看下一批时也不会丢掉上一页选的（分页只是把行藏起来，勾选状态还在）。
//   4. 提交时清掉这份记录：下发完回到概览页，不该还带着上次的选择。
function wireBatchSelection() {
  var form = document.getElementById("batch-form");
  var all = document.getElementById("dev-check-all");
  var count = document.getElementById("batch-count");
  var boxes = Array.prototype.slice.call(document.querySelectorAll(".dev-check"));
  if (!form || boxes.length === 0) return;
  var KEY = "acs-batch-ids";

  // 往上找所在的表格行（分页靠行元素的 style.display 控制）
  function rowOf(box) {
    var el = box;
    while (el && el.tagName !== "TR") el = el.parentNode;
    return el;
  }
  function visible(box) {
    var row = rowOf(box);
    return !row || row.style.display !== "none";
  }
  function save() {
    try {
      sessionStorage.setItem(KEY, JSON.stringify(
        boxes.filter(function (b) { return b.checked; }).map(function (b) { return b.value; })));
    } catch (e) { /* 隐私模式写不进去，忽略 */ }
  }
  function restore() {
    var saved;
    try { saved = JSON.parse(sessionStorage.getItem(KEY) || "[]"); } catch (e) { saved = []; }
    if (!saved.length) return;
    boxes.forEach(function (b) { b.checked = saved.indexOf(b.value) >= 0; });
  }
  function refresh() {
    var n = boxes.filter(function (b) { return b.checked; }).length;
    if (count) count.textContent = t("已选 %d 台").replace("%d", n);
    if (all) {
      var vis = boxes.filter(visible);
      all.checked = vis.length > 0 && vis.every(function (b) { return b.checked; });
    }
    var submit = form.querySelector("button[type=submit]");
    if (submit) submit.disabled = n === 0;
  }

  if (all) {
    all.addEventListener("change", function () {
      boxes.forEach(function (b) { if (visible(b)) b.checked = all.checked; });
      save();
      refresh();
    });
  }
  boxes.forEach(function (b) {
    b.addEventListener("change", function () { save(); refresh(); });
  });
  form.addEventListener("submit", function (e) {
    if (boxes.filter(function (b) { return b.checked; }).length === 0) {
      e.preventDefault();
      window.alert(t("先勾选要下发的设备"));
      return;
    }
    try { sessionStorage.removeItem(KEY); } catch (err) { /* 忽略 */ }
  });

  restore();
  refresh();
}

// ---------- 5 秒自动刷新（设备列表页 + 设备详情页，开关状态全局共享）----------
//
// 在线/离线、探测中、任务进度这些会自己变，盯着看的时候不用手动刷。
// 开关状态记在 localStorage（**所以两个页面共用一个开关**）：在一个页面打开，
// 切到另一个页面也是开着的，刷新后仍然保持。
// 三种情况会跳过这一轮（下一轮再看）：
//   1. 焦点在输入框里（正在打字）—— 别把没提交的搜索词刷掉；
//   2. 有弹窗开着 —— 别把正在看的终端列表/正在填的无线表单刷没；
//   3. 开着的时候页面不可见？不，后台标签照刷，切回来就是新的。
function wireAutoRefresh() {
  var btn = document.getElementById("autorefresh");
  if (!btn) return;
  var KEY = "acs-autorefresh";
  var SECONDS = 5;
  var on = false;
  var timer = null;
  try { on = localStorage.getItem(KEY) === "1"; } catch (e) { /* 隐私模式下读不到，当关 */ }

  function label() {
    btn.textContent = t(on ? "自动刷新 %ds：开" : "自动刷新 %ds：关").replace("%d", SECONDS);
    btn.classList.toggle("on", on);
    btn.setAttribute("aria-pressed", on ? "true" : "false");
  }

  function schedule() {
    if (timer) { clearTimeout(timer); timer = null; }
    if (!on) return;
    timer = setTimeout(function () {
      var el = document.activeElement;
      if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT")) {
        schedule();   // 正在输入，跳过这一轮
        return;
      }
      if (document.querySelector(".modal-mask:not([hidden])")) {
        schedule();   // 弹窗开着，跳过这一轮
        return;
      }
      window.location.reload();
    }, SECONDS * 1000);
  }

  label();
  schedule();

  btn.addEventListener("click", function () {
    on = !on;
    try { localStorage.setItem(KEY, on ? "1" : "0"); } catch (e) { /* 忽略 */ }
    label();
    schedule();
  });
}

// ---------- 终端弹窗 ----------
//
// 服务端把弹窗内容一起渲染在页面里（hidden），这里只管开/关 ——
// 不用额外请求，不依赖 JS 也能看到内容（只是要点开而已）。
function wireModals() {
  function close(mask) { mask.hidden = true; }

  document.querySelectorAll("[data-modal]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var mask = document.getElementById(btn.getAttribute("data-modal"));
      if (mask) mask.hidden = false;
    });
  });

  document.querySelectorAll(".modal-mask").forEach(function (mask) {
    // 点遮罩（而不是卡片本身）关闭
    mask.addEventListener("click", function (e) {
      if (e.target === mask) close(mask);
    });
    mask.querySelectorAll("[data-modal-close]").forEach(function (x) {
      x.addEventListener("click", function () { close(mask); });
    });
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll(".modal-mask").forEach(function (m) { close(m); });
  });
}

// ---------- 破坏性操作的二次确认 ----------
//
// 要确认的文字写在 form 的 data-confirm 属性上，不是拼在 JS 里：
// 属性由模板负责转义（设备名里带引号也不会把脚本搞坏），这里只负责弹框。
function wireConfirms() {
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      // 布尔项（启用/禁用这种）单独一个 checkbox 当「值」，很容易只勾了「改」
      // 却忘了勾值 —— 那种情况到底是想关还是忘勾，服务端分不出来，
      // 所以宁可拦下来让用户明确表态，也不要悄悄把无线关掉。
      var missing = boolFieldsMissingValue(form);
      if (missing.length) {
        e.preventDefault();
        window.alert(t("这些项勾了「改」但没勾「启用」，请补上或取消勾选：") + missing.join("、"));
        return;
      }
      if (!window.confirm(form.getAttribute("data-confirm"))) {
        e.preventDefault();
      }
    });
  });
}

// boolFieldsMissingValue 找出「只勾了 use_ 没勾值」的布尔项，返回它们的中文标签。
//
// 标签从表头那一列的 .wlabel 里取（那是模板渲染好的当前语言文本），
// 这样英文界面下提示也是英文。
function boolFieldsMissingValue(form) {
  var out = [];
  form.querySelectorAll("input.bool-value").forEach(function (valBox) {
    if (valBox.checked) return;
    var key = valBox.getAttribute("name"); // v_xxx
    var useBox = form.querySelector('input[name="use_' + key.slice(2) + '"]');
    if (!useBox || !useBox.checked) return;
    var row = valBox.parentNode;
    while (row && row !== form && !(row.classList && row.classList.contains("wrow"))) {
      row = row.parentNode;
    }
    var label = "";
    if (row) {
      var el = row.querySelector(".wlabel span");
      if (el) label = el.textContent.trim();
    }
    out.push(label || key.slice(2));
  });
  return out;
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
      ? t("🌙 切换到夜间") : t("☀ 切换到日间");
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

  // 自动刷新是整页重载，所以把"看到第几页 / 每页多少条"记在 sessionStorage 里，
  // 不然每 5 秒就被打回第一页（开着自动刷新看参数表时最难受）。
  var stateKey = table.id ? "acs-pager:" + table.id : "";
  if (stateKey) {
    try {
      var sSize = parseInt(sessionStorage.getItem(stateKey + ":size"), 10);
      var sPage = parseInt(sessionStorage.getItem(stateKey + ":page"), 10);
      if (!isNaN(sSize) && sSize >= 0) size = sSize;
      if (!isNaN(sPage) && sPage > 0) page = sPage;
    } catch (e) { /* 隐私模式读不到，用默认值 */ }
  }

  // 行数本来就不到一页时，分页条纯属噪音，直接不显示。
  if (rows.length <= defaultSize) {
    return { setFilter: function () {}, render: function () {} };
  }

  var bar = document.createElement("div");
  bar.className = "pager";

  var prev = mkBtn(t("‹ 上一页"));
  var next = mkBtn(t("下一页 ›"));
  var info = document.createElement("span");
  info.className = "pager-info";

  var sizeSel = document.createElement("select");
  sizeSel.className = "pager-size";
  [20, 50, 100, 0].forEach(function (n) {
    var o = document.createElement("option");
    o.value = String(n);
    o.textContent = n === 0 ? t("全部") : t("每页 %d 条").replace("%d", n);
    sizeSel.appendChild(o);
  });
  sizeSel.value = String(size);

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
      ? t("共 %d 条 · 第 %d / %d 页").replace("%d", list.length).replace("%d", page).replace("%d", pages)
      : t("共 %d 条").replace("%d", list.length);
    prev.disabled = page <= 1;
    next.disabled = page >= pages;

    if (stateKey) {
      try {
        sessionStorage.setItem(stateKey + ":page", String(page));
        sessionStorage.setItem(stateKey + ":size", String(size));
      } catch (e) { /* 忽略 */ }
    }
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

  // 过滤词同样记在 sessionStorage：自动刷新重载后还在，不用每 5 秒重打一遍
  var filterKey = "acs-param-filter";
  try {
    var savedFilter = sessionStorage.getItem(filterKey);
    if (savedFilter) box.value = savedFilter;
  } catch (e) { /* 忽略 */ }

  box.addEventListener("input", function () {
    try { sessionStorage.setItem(filterKey, box.value); } catch (e) { /* 忽略 */ }
    apply();
  });
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
