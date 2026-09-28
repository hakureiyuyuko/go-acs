// 界面上的两处小交互：参数表过滤、概览页搜索框。
// 纯原生 JS，没有依赖。
document.addEventListener("DOMContentLoaded", function () {
  paramFilter();
  searchBox();
});

// 参数表过滤：按参数名做即时筛选。
function paramFilter() {
  var box = document.getElementById("param-filter");
  var count = document.getElementById("param-count");
  if (!box) return;
  var rows = Array.prototype.slice.call(document.querySelectorAll("#param-table tbody tr"));

  function apply() {
    var q = box.value.trim().toLowerCase();
    var shown = 0;
    rows.forEach(function (tr) {
      var hit = !q || tr.getAttribute("data-name").toLowerCase().indexOf(q) >= 0;
      tr.style.display = hit ? "" : "none";
      if (hit) shown++;
    });
    if (count) count.textContent = "显示 " + shown + " / " + rows.length + " 条";
  }

  box.addEventListener("input", apply);
  apply();
}

// 概览页搜索：输入后停顿一下自动提交（服务端过滤，结果 URL 可分享）。
// 刷新后把光标放回输入框，让连续输入不被打断。
function searchBox() {
  var box = document.getElementById("dev-search");
  if (!box || !box.form) return;

  if (box.value) {
    box.focus();
    try { box.setSelectionRange(box.value.length, box.value.length); } catch (e) { /* 忽略 */ }
  }

  var timer = null;
  box.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { box.form.submit(); }, 350);
  });
}
