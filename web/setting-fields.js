import { el } from "./rendering.js";

export function settingControl(f, prefix = "ss") {
  let input;
  switch (f.kind) {
    case "switch":
      input = el("input", "switch" + (f.exposes ? " exposes" : ""));
      input.type = "checkbox";
      input.setAttribute("role", "switch");
      break;
    case "number":
      input = el("input", "input mono rg-num");
      input.type = "number";
      if (f.min !== undefined) input.min = String(f.min);
      if (f.step) input.step = String(f.step);
      break;
    case "port":
      input = el("input", "input mono ss-port");
      input.type = "number";
      input.min = "1";
      input.max = "65535";
      input.inputMode = "numeric";
      break;
    case "select":
      input = el("select", "input");
      break;
    case "lines":
      input = el("textarea", "input mono");
      input.rows = 6;
      input.spellcheck = false;
      break;
    case "readonly":
      input = el("code", "ss-ro");
      break;
    default:
      input = el("input", "input mono");
      input.autocomplete = "off";
      input.spellcheck = false;
  }
  if (f.placeholder) input.placeholder = f.placeholder;
  input.id = `${prefix}-${f.key}`;
  return input;
}

export function settingRow(f, input) {
  const r = el("div", "ss-row" + (f.sub ? " ss-sub" : "") + (f.kind === "lines" ? " ss-wide" : ""));
  r.dataset.key = f.key;
  const text = el("div", "ss-text");
  const name = el("label", "ss-name", f.label);
  name.htmlFor = input.id;
  if (f.help) {
    const q = el("span", "ss-q", "?");
    q.title = f.help;
    q.tabIndex = 0;
    q.setAttribute("role", "img");
    q.setAttribute("aria-label", f.help);
    name.append(q);
  }
  text.append(name);
  if (f.desc) {
    const d = el("div", "ss-desc", f.desc);
    d.id = `${input.id}-desc`;
    input.setAttribute("aria-describedby", d.id);
    text.append(d);
  }
  if (f.prefix) {
    const addr = el("span", "ss-addr");
    const host = el("span", "mono ss-host", "127.0.0.1:");
    host.id = `${input.id}-host`;
    addr.append(host, input);
    r.append(text, addr);
    return r;
  }
  r.append(text, input);
  return r;
}

