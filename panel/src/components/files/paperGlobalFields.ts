import type { ConfigFieldSpec } from "./configFields";

export const PAPER_GLOBAL_FIELDS: readonly ConfigFieldSpec[] = [
  { id: "global_auto_send", path: ["chunk-loading-advanced", "auto-config-send-distance"], type: "boolean" },
  { id: "global_player_collisions", path: ["collisions", "enable-player-collisions"], type: "boolean" },
  { id: "global_collision_position", path: ["collisions", "send-full-pos-for-hard-colliding-entities"], type: "boolean" },
  { id: "global_suggest_names", path: ["commands", "suggest-player-names-when-null-tab-completions"], type: "boolean" },
  { id: "global_console_completion", path: ["console", "enable-brigadier-completions"], type: "boolean" },
  { id: "global_console_highlight", path: ["console", "enable-brigadier-highlighting"], type: "boolean" },
  { id: "global_quit_name", path: ["messages", "use-display-name-in-quit-message"], type: "boolean" },
  { id: "global_empty_teams", path: ["scoreboards", "save-empty-scoreboard-teams"], type: "boolean" },
  { id: "global_plugin_scoreboards", path: ["scoreboards", "track-plugin-scoreboards"], type: "boolean" },
  { id: "global_concurrent_generates", path: ["chunk-loading-advanced", "player-max-concurrent-chunk-generates"], type: "integer", min: -1, max: 2147483647 },
  { id: "global_concurrent_loads", path: ["chunk-loading-advanced", "player-max-concurrent-chunk-loads"], type: "integer", min: -1, max: 2147483647 },
  { id: "global_autosave_budget", path: ["player-auto-save", "max-per-tick"], type: "integer", min: -1, max: 2147483647 },
  { id: "global_autosave_rate", path: ["player-auto-save", "rate"], type: "integer", min: -1, max: 2147483647 },
  { id: "global_watchdog_delay", path: ["watchdog", "early-warning-delay"], type: "integer", min: 0, max: 2147483647 },
  { id: "global_watchdog_every", path: ["watchdog", "early-warning-every"], type: "integer", min: 0, max: 2147483647 },
  { id: "global_generate_rate", path: ["chunk-loading-basic", "player-max-chunk-generate-rate"], type: "number", min: 0, allowMinusOne: true },
  { id: "global_load_rate", path: ["chunk-loading-basic", "player-max-chunk-load-rate"], type: "number", min: 0, allowMinusOne: true },
  { id: "global_send_rate", path: ["chunk-loading-basic", "player-max-chunk-send-rate"], type: "number", min: 0, allowMinusOne: true },
  { id: "global_no_permission", path: ["messages", "no-permission"], type: "text", maxBytes: 4096 },
  { id: "global_throttle_message", path: ["messages", "kick", "connection-throttle"], type: "text", maxBytes: 4096 },
];
