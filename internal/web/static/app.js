// 参数表过滤：纯原生 JS，无依赖。
document.addEventListener("DOMContentLoaded", function () {
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
});
