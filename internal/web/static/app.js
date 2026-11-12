// SPDX-License-Identifier: AGPL-3.0-or-later
// Small behaviours HTMX does not cover. No inline handlers: the CSP forbids them.
(function () {
  // Errors (4xx/5xx) are not swapped into the page; their text goes to a toast.
  htmx.config.responseHandling = [
    { code: "204", swap: false },
    { code: "[23]..", swap: true },
    { code: "[45]..", swap: false, error: true },
  ];

  function toast(message, isError) {
    const box = document.getElementById("toast");
    if (!box || !message) return;
    const el = document.createElement("div");
    el.className = "toast" + (isError ? " error" : "");
    el.textContent = message.trim().slice(0, 400);
    box.appendChild(el);
    setTimeout(() => el.remove(), isError ? 8000 : 4000);
  }

  document.addEventListener("htmx:responseError", (e) => {
    if (e.detail.xhr.status === 401) return; // HX-Redirect sends us to sign in
    toast(e.detail.xhr.responseText || "Request failed", true);
  });
  document.addEventListener("htmx:sendError", () => toast("Cannot reach Drawbridge.", true));
  // Servers send HX-Trigger: {"toast": "message"}.
  document.addEventListener("toast", (e) => toast(e.detail.value));

  // Appearance: show a new theme or mode at once; the form saves it.
  document.addEventListener("change", (e) => {
    const form = e.target.closest("#appearance-form");
    if (!form) return;
    document.documentElement.dataset.theme = form.elements.theme.value;
    document.documentElement.dataset.mode = form.elements.mode.value;
  });

  // Settings tabs scroll sideways on a narrow screen: bring the open one into view.
  document.addEventListener("DOMContentLoaded", () => {
    const tab = document.querySelector('.tabs [aria-current="page"]');
    if (tab) tab.parentElement.scrollLeft = tab.offsetLeft - (tab.parentElement.clientWidth - tab.offsetWidth) / 2;
  });

  // A select-all checkbox checks every box with the given name in its table.
  document.addEventListener("change", (e) => {
    const all = e.target.closest("[data-select-all]");
    if (all) {
      const name = all.getAttribute("data-select-all");
      all.closest("table").querySelectorAll(`input[name="${name}"]`).forEach((b) => (b.checked = all.checked));
    }
    const auto = e.target.closest("[data-autosubmit]");
    if (auto && auto.form) auto.form.requestSubmit();
  });

  // The account menu and a phone's More menu close on a click elsewhere or on Escape.
  const menus = "details.usermenu[open], details.more[open]";
  document.addEventListener("click", (e) => {
    document.querySelectorAll(menus).forEach((d) => {
      if (!d.contains(e.target)) d.open = false;
    });
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    document.querySelectorAll(menus).forEach((d) => {
      d.open = false;
      d.querySelector("summary").focus();
    });
  });

  // Forms show only the fields that apply: notification channels by their
  // kind (data-kinds), the test schedule by how often it runs (data-every).
  const choosers = { kind: "data-kinds", every: "data-every" };
  function showFields(select) {
    const attr = choosers[select.name];
    select.form.querySelectorAll(`[${attr}]`).forEach((el) => {
      el.hidden = !el.getAttribute(attr).split(" ").includes(select.value);
    });
  }
  const chooser = 'select[name="kind"], select[name="every"]';
  document.addEventListener("change", (e) => {
    if (e.target.matches(chooser) && e.target.form) showFields(e.target);
  });
  document.addEventListener("htmx:load", (e) => {
    e.detail.elt.querySelectorAll(chooser).forEach((s) => s.form && showFields(s));
  });

  // Buttons marked data-collapse load their target once, then a second click
  // empties it again; the attribute holds the label while it is open.
  document.addEventListener("htmx:beforeRequest", (e) => {
    const btn = e.detail.elt;
    if (!btn.matches || !btn.matches("[data-collapse]") || btn.getAttribute("aria-expanded") !== "true") return;
    e.preventDefault();
    e.detail.target.replaceChildren();
    btn.textContent = btn.dataset.expand;
    btn.setAttribute("aria-expanded", "false");
  });
  document.addEventListener("htmx:afterSwap", (e) => {
    const btn = e.detail.requestConfig && e.detail.requestConfig.elt;
    if (!btn || !btn.matches || !btn.matches("[data-collapse]")) return;
    btn.dataset.expand = btn.textContent;
    btn.textContent = btn.dataset.collapse;
    btn.setAttribute("aria-expanded", "true");
  });

  // The dashboard's last check counts up every second, with the time to the
  // next check and a bar filling towards it. The status card is replaced on
  // each check, so this reads the elements afresh each time.
  function ago(sec) {
    if (sec < 10) return "just now";
    if (sec < 60) return sec + "s ago";
    return Math.floor(sec / 60) + "m ago";
  }
  function tick() {
    document.querySelectorAll("time[data-live]").forEach((t) => {
      const sec = Math.max(0, Math.floor((Date.now() - Date.parse(t.dataset.live)) / 1000));
      if (sec >= 3600) return; // older checks keep the server's wording
      t.textContent = ago(sec);
      const every = Number(t.dataset.every);
      const tile = t.closest(".tile");
      if (!every || !tile) return;
      const left = every - sec;
      tile.querySelector(".next-check").textContent = left > 0 ? "next in " + left + "s" : "checking…";
      tile.querySelector(".check-progress").style.width = Math.min(100, (sec / every) * 100) + "%";
    });
  }
  setInterval(tick, 1000);
  document.addEventListener("DOMContentLoaded", tick);
  document.addEventListener("htmx:afterSwap", tick);

  // The jump box: ⌘K or Ctrl+K (or the sidebar button) opens it; arrow keys
  // move between the results and Enter opens the chosen one.
  function openJump() {
    const dlg = document.getElementById("jump");
    if (!dlg || dlg.open) return;
    const input = dlg.querySelector("input");
    input.value = "";
    dlg.showModal();
    input.focus();
    htmx.trigger(input, "jump-open");
  }
  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      openJump();
    }
  });
  document.addEventListener("click", (e) => {
    if (e.target.closest("[data-jump]")) openJump();
    if (e.target.id === "jump") e.target.close(); // a click on the backdrop
  });
  document.addEventListener("keydown", (e) => {
    const dlg = e.target.closest && e.target.closest("#jump");
    if (!dlg || !["ArrowDown", "ArrowUp", "Enter"].includes(e.key)) return;
    const links = [...dlg.querySelectorAll("#jump-results a")];
    const at = links.findIndex((a) => a.classList.contains("active"));
    if (e.key === "Enter") {
      e.preventDefault();
      if (links[at]) links[at].click();
      return;
    }
    e.preventDefault();
    const next = links[(at + (e.key === "ArrowDown" ? 1 : -1) + links.length) % links.length];
    links.forEach((a) => a.classList.toggle("active", a === next));
    if (next) next.scrollIntoView({ block: "nearest" });
  });

  // An open stack card stays open when the list refreshes.
  let openStacks = [];
  document.addEventListener("htmx:beforeSwap", (e) => {
    if (e.detail.target.id === "stacks-table") {
      openStacks = [...e.detail.target.querySelectorAll("details.stack-card[open]")].map((d) => d.id);
    }
  });
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id !== "stacks-table" && !document.getElementById("stacks-table")) return;
    openStacks.forEach((id) => {
      const d = document.getElementById(id);
      if (d) d.open = true;
    });
  });

  // Forms marked data-reset-on-success clear after a successful HTMX post.
  document.addEventListener("htmx:afterRequest", (e) => {
    const form = e.detail.elt.closest && e.detail.elt.closest("form[data-reset-on-success]");
    if (form && e.detail.successful) form.reset();
  });
})();
