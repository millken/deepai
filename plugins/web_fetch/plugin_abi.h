// plugin_abi.h — DeepAI shared library plugin ABI definitions.
//
// This is the authoritative C header for plugin authors. Do NOT use
// CGO-generated headers (e.g. web_fetch.h) — they are internal artifacts
// and may lag behind the actual ABI.
//
// Version: ABI 1.0
// Date:    2026-04-05

#ifndef DEEPAI_PLUGIN_ABI_H
#define DEEPAI_PLUGIN_ABI_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// ── Required symbols ──────────────────────────────────────────

/// Return the ABI version string. Must return "1.0" to match the host.
/// The returned string must be dynamically allocated (e.g. via malloc/C.CString).
/// The host will call plugin_free_string to release it after copying.
/// Do NOT return a static string literal — the host assumes ownership.
char* plugin_abi_version(void);

/// Create a new plugin instance. Returns an opaque pointer that must be
/// passed to all other plugin_* functions.
uintptr_t plugin_new(void);

/// Return the plugin name.
char* plugin_name(uintptr_t ptr);

/// Return the plugin version.
char* plugin_version(uintptr_t ptr);

/// Return the plugin description.
char* plugin_description(uintptr_t ptr);

// ── Optional lifecycle symbols ─────────────────────────────────

/// Return the plugin type string (e.g. "tool", "llm", "hook").
char* plugin_type(uintptr_t ptr);

/// Initialize the plugin with a JSON config string.
void plugin_init(uintptr_t ptr, const char* config_json);

/// Start the plugin.
void plugin_start(uintptr_t ptr);

/// Stop the plugin.
void plugin_stop(uintptr_t ptr);

/// Release all resources. Called once before dlclose.
void plugin_close(uintptr_t ptr);

// ── Tool symbols ──────────────────────────────────────────────

/// Return tool definitions as a JSON array of ToolDef objects.
char* plugin_tools(uintptr_t ptr);

/// Execute a tool. call_id uniquely identifies this invocation for
/// precise cancellation via plugin_cancel.
///
/// Returns a JSON string owned by the plugin. The host will call
/// plugin_free_string to release it after copying.
char* plugin_execute(uintptr_t ptr, const char* tool_name,
                      const char* args_json, uint64_t call_id);

/// Free a C string previously returned by plugin_execute or plugin_tools.
void plugin_free_string(char* s);

/// Cancel a specific in-flight call identified by call_id.
void plugin_cancel(uintptr_t ptr, uint64_t call_id);

#ifdef __cplusplus
}
#endif

#endif // DEEPAI_PLUGIN_ABI_H
