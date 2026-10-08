// Dashboard initialization order is significant; import order is not.

import { initConnect } from "./connect.js";
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
initConnect();
initWorkloadDialog();
initDoctor();
initServerSettings();
initLive();
tick();
// Mesh topology, peer Activity, and peers without feeds still need polling.
// Pause it while the tab is hidden.
setInterval(() => { if (!document.hidden) void tick(); }, POLL_MS);
