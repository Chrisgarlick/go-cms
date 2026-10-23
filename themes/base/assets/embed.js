// Click-to-load embeds: nothing from the provider loads until the visitor asks.
document.addEventListener("click", function (e) {
  var a = e.target.closest && e.target.closest(".embed-facade");
  if (!a) return;
  var box = a.closest(".embed");
  var src = box && box.getAttribute("data-embed-src");
  if (!src || src.indexOf("https://") !== 0) return;
  e.preventDefault();
  var f = document.createElement("iframe");
  f.src = src;
  f.title = box.getAttribute("data-embed-title") || "Embedded content";
  f.allow = "autoplay; fullscreen; picture-in-picture; encrypted-media";
  f.setAttribute("sandbox", "allow-scripts allow-same-origin allow-presentation allow-popups");
  f.setAttribute("allowfullscreen", "");
  f.referrerPolicy = "strict-origin-when-cross-origin";
  box.replaceChildren(f);
  f.focus();
});
