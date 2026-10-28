// Draws a release process as lanes and animates it: each lane is one reason a
// run starts (a trigger) and the steps that follow, built left to right with
// each step turning to its outcome. A step that fans out is an array.
//
//   require("lanes")(dottie, {lanes: [
//     {trigger: {text: "push to main"}, steps: [{text: "test", state: "pass"}, ...]},
//   ]})
//
// states: pass (green), fail (red), effect (amber: a step with outside effects),
// plain (unchanged). A step with `free: true` is not connected to the one before
// it: it does not wait for it. Used through Dottie's run_script, see dev/animations/README.md.
module.exports = function (dottie, spec) {
  const W = spec.width || 178, H = 56, GX = spec.gap || 70, ROW = 76, GAP = 40, STEP = 0.95;
  const fills = { trigger: "#dbeafe", pass: "#d1fae5", fail: "#fecaca", effect: "#fde68a", plain: "#ffffff" };
  let y = 0, t = 0;
  for (const lane of spec.lanes) {
    const trigger = Object.assign({ state: "trigger" }, lane.trigger);
    const cols = [trigger].concat(lane.steps).map(c => (Array.isArray(c) ? c : [c]));
    const rows = Math.max.apply(null, cols.map(c => c.length));
    const laneH = rows * ROW;
    let prev = [];
    cols.forEach((col, k) => {
      const tk = t + k * STEP;
      const x = k * (W + GX);
      const ids = col.map((n, i) => {
        const cy = y + (laneH - col.length * ROW) / 2 + i * ROW;
        const stateful = n.state && n.state !== "trigger" && n.state !== "plain";
        const el = dottie.rect({
          x: x, y: cy, width: n.w || W, height: H, text: n.text,
          fill: stateful ? fills.plain : fills[n.state || "plain"],
          font: "Caveat", fontSize: 22,
        });
        dottie.transition(el, "popIn", tk, { duration: 0.4 });
        if (stateful) {
          dottie.key(el, tk + 0.5, { fill: fills.plain });
          dottie.key(el, tk + 0.9, { fill: fills[n.state] });
          dottie.transition(el, n.state === "fail" ? "wiggle" : "pulse", tk + 0.5, { duration: 0.45 });
        }
        (n.free ? [] : prev).forEach(p => {
          const c = dottie.connect(p, el, { routing: prev.length > 1 || col.length > 1 ? "elbow" : "straight" });
          dottie.transition(c, "drawIn", tk - 0.45, { duration: 0.45 });
        });
        return el;
      });
      prev = ids;
    });
    t += (cols.length - 1) * STEP + 1.7;
    y += laneH + GAP;
  }
  dottie.timeline({ fps: 30, duration: t + 1.2, boil: 0 });
  return { duration: t + 1.2, lanes: spec.lanes.length };
};
