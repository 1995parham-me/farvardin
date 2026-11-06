// farvardin: progressive enhancements. Every page works without this.
(() => {
  // "/" focuses the search box; Escape leaves it.
  document.addEventListener("keydown", (e) => {
    const t = e.target;
    const typing = t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement;
    if (e.key === "/" && !typing) {
      const box = document.querySelector("[data-filter]") || document.querySelector("[data-hotkey='/']");
      if (box) { e.preventDefault(); box.focus(); box.select(); }
    } else if (e.key === "Escape" && typing) {
      t.blur();
    }
  });

  // Instant filtering of the contact list as you type.
  const filter = document.querySelector("[data-filter]");
  if (filter) {
    const items = [...document.querySelectorAll(filter.dataset.filter)];
    const groups = [...document.querySelectorAll("[data-group]")];
    const count = document.querySelector("[data-count]");
    const none = document.querySelector("[data-none]");
    const apply = () => {
      const q = filter.value.trim().toLowerCase();
      let shown = 0;
      for (const li of items) {
        const hit = !q || li.dataset.search.includes(q);
        li.hidden = !hit;
        if (hit) shown++;
      }
      for (const g of groups) g.hidden = !g.querySelector("li:not([hidden])");
      if (count) count.textContent = shown;
      if (none) none.hidden = shown !== 0;
    };
    filter.addEventListener("input", apply);
    // Pressing Enter opens the only match.
    filter.form.addEventListener("submit", (e) => {
      const visible = items.filter((li) => !li.hidden);
      if (visible.length === 1) { e.preventDefault(); location.href = visible[0].querySelector("a").href; }
    });
    if (filter.value) apply();
  }

  // Copy buttons.
  for (const b of document.querySelectorAll("[data-copy]")) {
    b.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(b.dataset.copy);
        b.classList.add("done");
        setTimeout(() => b.classList.remove("done"), 1200);
      } catch { /* clipboard needs HTTPS; nothing to do */ }
    });
  }
})();
