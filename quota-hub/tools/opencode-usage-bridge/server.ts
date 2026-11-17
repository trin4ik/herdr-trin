import { Plugin } from "@opencode/plugin"
import { PLUGIN_ID, setupBridge } from "./bridge.mjs"

export default Plugin.define({
  id: PLUGIN_ID,
  async setup(ctx) {
    return await setupBridge(ctx)
  },
})
