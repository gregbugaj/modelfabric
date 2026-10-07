import { $ } from "./core.js";

const THEME_KEY = "mfsh.theme";

function applyTheme(theme) {
  if (theme === "light" || theme === "dark") {
    document.documentElement.setAttribute("data-theme", theme);
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
  $("theme-label").textContent =
    theme === "light" ? "Light" : theme === "dark" ? "Dark" : "System";
}

export function initTheme() {
  let theme = null;
  try {
    theme = localStorage.getItem(THEME_KEY);
  } catch {
    // Private windows and blocked storage can throw; fall back to the OS theme.
  }
  applyTheme(theme);
  $("theme-toggle").addEventListener("click", () => {
    const order = [null, "light", "dark"];
    const current = document.documentElement.getAttribute("data-theme");
    const next = order[(order.indexOf(current) + 1) % order.length];
    applyTheme(next);
    try {
      next ? localStorage.setItem(THEME_KEY, next) : localStorage.removeItem(THEME_KEY);
    } catch {
    }
  });
}
