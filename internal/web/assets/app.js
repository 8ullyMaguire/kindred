/* kindred -- progressive enhancement only.
 *
 * Every feature here has a no-JS path. The search box is a real <form>
 * with method=get, so it works if this file fails to load, and a browser
 * with JavaScript disabled loses nothing it cannot get back by following
 * a link.
 *
 * No framework, no bundler, no build step. The target host has 512 MB of
 * RAM and the whole binary is 12 MB; a node_modules tree would be the
 * largest thing in the deployment by an order of magnitude.
 */
(function () {
  "use strict";

  /* -- multi-seed recommender -------------------------------------------
   *
   * The recommender accepts several seeds at once, which is the thing that
   * makes it a recommender rather than a similarity lookup: give it two
   * works you liked and it ranks the intersection of their
   * neighbourhoods. The API has always supported this; the single-seed UI
   * just never exposed it.
   */
  function seedList(root) {
    var seeds = root.querySelector("[data-seeds]");
    if (!seeds) return null;
    return {
      el: seeds,
      values: Array.prototype.map.call(
        seeds.querySelectorAll("input[type=hidden]"),
        function (i) { return i.value; }
      ),
      add: function (id) {
        if (this.values.indexOf(id) !== -1) return false;
        var input = document.createElement("input");
        input.type = "hidden";
        input.name = "seed";
        input.value = id;
        this.el.appendChild(input);
        this.values.push(id);
        return true;
      }
    };
  }

  function wireSeeds() {
    var root = document.querySelector("[data-seedable]");
    var seeds = seedList(root);
    if (!seeds) return;

    root.addEventListener("click", function (ev) {
      var btn = ev.target.closest("[data-seed]");
      if (!btn) return;
      ev.preventDefault();

      var id = btn.getAttribute("data-seed");
      if (!seeds.add(id)) return;

      btn.textContent = "added as a seed";
      btn.setAttribute("disabled", "disabled");
      btn.classList.add("muted");

      var form = document.getElementById("seed-form");
      if (form) form.submit();
    });
  }

  /* -- copy the API endpoint -------------------------------------------
   *
   * Useful and tiny: the API is the product, and a person who wants to
   * script against it should not have to read the source to find the
   * base URL.
   */
  function wireCopy() {
    var btn = document.querySelector("[data-copy]");
    if (!btn) return;

    btn.addEventListener("click", function () {
      var text = btn.getAttribute("data-copy");
      var done = function () {
        var was = btn.textContent;
        btn.textContent = "copied";
        setTimeout(function () { btn.textContent = was; }, 1200);
      };

      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {});
        return;
      }
      // No clipboard API, or it was refused (it needs a secure context,
      // and a LAN http:// origin does not have one). Fall back to a
      // selection the user can copy themselves.
      var ta = document.createElement("textarea");
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); done(); } catch (e) {}
      document.body.removeChild(ta);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () {
      wireSeeds();
      wireCopy();
    });
  } else {
    wireSeeds();
    wireCopy();
  }
})();
