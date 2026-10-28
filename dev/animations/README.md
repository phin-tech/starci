# The release-process animations

`docs/animations/<example>.dottie` is a drawing of one example's release
process, played on that example's page by [dottie.js](https://dottie.phin.tech/dottie-js/).
An example gets an animation by having a drawing named for it there; the other
basic examples get their plan as a graph instead.

Open one in Dottie to change it. To start one, or to make the five alike, use
`lanes.js`: it draws a list of lanes, each a reason a run starts and the steps
that follow, and animates them in turn. Give it to Dottie's `run_script` as a
module, then:

```js
require("lanes")(dottie, {lanes: [
  {trigger: {text: "push to main"}, steps: [{text: "test", state: "pass"}, {text: "publish", state: "effect"}]},
]});
```

A step's `state` is `pass`, `fail`, `effect` (amber: it has effects outside the
run, so a pass is never reused) or `plain`; an array in `steps` is a fan-out; a
step with `free: true` is not connected to the one before it. `width` and `gap`
at the top level size the nodes.

Draw only what the script does. `lanes.js` does not read the script, so a change
to an example's behaviour is a change to its drawing too.

The player is pinned in `mkdocs.yml`. It loads the handwriting fonts from beside
its own script on the CDN.
