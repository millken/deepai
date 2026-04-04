//! Weather Tool Plugin - Rust Implementation
//!
//! This demonstrates how to implement a DeepAI plugin in Rust.
//! The plugin exports C-compatible functions that can be loaded by the purego-based loader.
//!
//! Build with: cargo build --release --crate-type cdylib

use std::ffi::{CStr, CString};
use std::os::raw::c_char;
use std::ptr;
use serde::{Deserialize, Serialize};

/// Plugin instance state
struct WeatherPlugin {
    api_key: Option<String>,
    cache_ttl: u64,
}

/// Tool definition schema
#[derive(Serialize)]
struct ToolDef {
    name: &'static str,
    description: &'static str,
    input_schema: serde_json::Value,
}

/// Weather query arguments
#[derive(Deserialize)]
struct WeatherArgs {
    city: String,
    #[serde(default)]
    unit: String,
    #[serde(default)]
    days: Option<u8>,
}

/// Weather result
#[derive(Serialize)]
struct WeatherResult {
    content: String,
    temperature: f64,
    condition: String,
}

/// Global plugin instance (simplified - real implementation should use a map)
static mut PLUGIN: Option<WeatherPlugin> = None;

// ============== Exported C Functions ==============

/// Create a new plugin instance
#[no_mangle]
pub extern "C" fn plugin_new() -> *mut () {
    unsafe {
        PLUGIN = Some(WeatherPlugin {
            api_key: None,
            cache_ttl: 300,
        });
    }
    ptr::null_mut() // Return null pointer as instance (simplified)
}

/// Get plugin name
#[no_mangle]
pub extern "C" fn plugin_name(_ptr: *mut ()) -> *mut c_char {
    CString::new("weather-tool")
        .unwrap()
        .into_raw()
}

/// Get plugin version
#[no_mangle]
pub extern "C" fn plugin_version(_ptr: *mut ()) -> *mut c_char {
    CString::new("1.0.0")
        .unwrap()
        .into_raw()
}

/// Get plugin description
#[no_mangle]
pub extern "C" fn plugin_description(_ptr: *mut ()) -> *mut c_char {
    CString::new("Get weather information for any city")
        .unwrap()
        .into_raw()
}

/// Get plugin type
#[no_mangle]
pub extern "C" fn plugin_type(_ptr: *mut ()) -> *mut c_char {
    CString::new("tool")
        .unwrap()
        .into_raw()
}

/// Initialize plugin with JSON config
#[no_mangle]
pub extern "C" fn plugin_init(_ptr: *mut (), config_json: *const c_char) {
    if config_json.is_null() {
        return;
    }

    let config_str = unsafe { CStr::from_ptr(config_json) };
    if let Ok(json_str) = config_str.to_str() {
        if let Ok(config) = serde_json::from_str::<serde_json::Value>(json_str) {
            unsafe {
                if let Some(ref mut plugin) = PLUGIN {
                    if let Some(api_key) = config.get("api_key").and_then(|v| v.as_str()) {
                        plugin.api_key = Some(api_key.to_string());
                    }
                    if let Some(cache_ttl) = config.get("cache_ttl").and_then(|v| v.as_u64()) {
                        plugin.cache_ttl = cache_ttl;
                    }
                }
            }
        }
    }
}

/// Start plugin (optional)
#[no_mangle]
pub extern "C" fn plugin_start(_ptr: *mut ()) {
    // No-op
}

/// Stop plugin (optional)
#[no_mangle]
pub extern "C" fn plugin_stop(_ptr: *mut ()) {
    // No-op
}

/// Close plugin and release resources
#[no_mangle]
pub extern "C" fn plugin_close(_ptr: *mut ()) {
    unsafe {
        PLUGIN = None;
    }
}

/// Get tool definitions as JSON
#[no_mangle]
pub extern "C" fn plugin_tools(_ptr: *mut ()) -> *mut c_char {
    let tools = vec![
        ToolDef {
            name: "get_weather",
            description: "Get current weather for a city",
            input_schema: serde_json::json!({
                "type": "object",
                "properties": {
                    "city": {
                        "type": "string",
                        "description": "City name"
                    },
                    "unit": {
                        "type": "string",
                        "enum": ["celsius", "fahrenheit"],
                        "default": "celsius"
                    }
                },
                "required": ["city"]
            }),
        },
        ToolDef {
            name: "get_forecast",
            description: "Get weather forecast for upcoming days",
            input_schema: serde_json::json!({
                "type": "object",
                "properties": {
                    "city": {
                        "type": "string",
                        "description": "City name"
                    },
                    "days": {
                        "type": "integer",
                        "description": "Number of days (1-7)",
                        "minimum": 1,
                        "maximum": 7,
                        "default": 3
                    }
                },
                "required": ["city"]
            }),
        },
    ];

    let json = serde_json::to_string(&tools).unwrap_or_else(|_| "[]".to_string());
    CString::new(json).unwrap().into_raw()
}

/// Execute a tool with JSON arguments, returns JSON result
#[no_mangle]
pub extern "C" fn plugin_execute(_ptr: *mut (), tool_name: *const c_char, args_json: *const c_char) -> *mut c_char {
    let tool = if tool_name.is_null() {
        return error_result("tool_name is null");
    } else {
        unsafe { CStr::from_ptr(tool_name) }
    };

    let args = if args_json.is_null() {
        serde_json::Value::Null
    } else {
        let args_str = unsafe { CStr::from_ptr(args_json) };
        serde_json::from_str(args_str.to_str().unwrap_or("{}")).unwrap_or(serde_json::Value::Null)
    };

    let result = match tool.to_str() {
        Ok("get_weather") => execute_get_weather(&args),
        Ok("get_forecast") => execute_get_forecast(&args),
        _ => error_result(&format!("Unknown tool: {:?}", tool)),
    };

    CString::new(result).unwrap().into_raw()
}

// ============== Internal Implementation ==============

fn error_result(msg: &str) -> String {
    serde_json::json!({ "error": msg }).to_string()
}

fn execute_get_weather(args: &serde_json::Value) -> String {
    let city = args.get("city").and_then(|v| v.as_str()).unwrap_or("Unknown");

    // In a real implementation, call weather API here
    let result = WeatherResult {
        content: format!("Weather in {}: Sunny, 25°C", city),
        temperature: 25.0,
        condition: "Sunny".to_string(),
    };

    serde_json::to_string(&result).unwrap_or_else(|_| error_result("Failed to serialize result"))
}

fn execute_get_forecast(args: &serde_json::Value) -> String {
    let city = args.get("city").and_then(|v| v.as_str()).unwrap_or("Unknown");
    let days = args.get("days").and_then(|v| v.as_u64()).unwrap_or(3) as usize;

    // In a real implementation, call weather API here
    let forecast: Vec<_> = (0..days)
        .map(|i| format!("Day {}: Partly cloudy, 22°C", i + 1))
        .collect();

    serde_json::json!({
        "city": city,
        "forecast": forecast
    }).to_string()
}
