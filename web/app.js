// The dashboard entry point. Each import below is a section of what was
// one 4,500-line file; the order of these imports does not matter, but the
// init calls at the bottom do — they are why this file still exists.

import "./theme.js";
import "./navigation.js";
import "./activity.js";
import "./rendering.js";
import "./settings-presets.js";
import "./routing.js";
import "./doctor.js";
import "./workload-presets.js";
import "./vision.js";
import "./runtime.js";
import "./actions.js";
import "./my-models.js";
import "./tune-slots.js";
import "./discover.js";
import "./mesh.js";
import "./polling.js";
import "./serversettings.js";
import "./bench.js";

import { POLL_MS } from "./core.js";
import { initDoctor } from "./doctor.js";
import { initCapture, initNav } from "./navigation.js";
import { initLive, tick } from "./polling.js";
import { initTheme } from "./theme.js";
import { initServerSettings } from "./serversettings.js";
import { initWorkloadDialog } from "./vision.js";

initTheme();
initNav();
initCapture();
initWorkloadDialog();
initDoctor();
initServerSettings();
initLive();
tick();
// Still on a timer with the feed open: the mesh map, the mesh-wide activity
// view and peers too old for the feed are only ever fetched. Not while the
// tab is hidden, when nothing on it is being looked at.
setInterval(() => { if (!document.hidden) void tick(); }, POLL_MS);
