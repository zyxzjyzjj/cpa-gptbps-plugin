// Command codex-bps is a native CPA plugin. Build with -buildmode=c-shared.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// Copy the host table: the pointer passed to init need not outlive init.
static cliproxy_host_api bps_host;
static void bps_store_host(const cliproxy_host_api* host) { bps_host = *host; }
static int bps_call_host(const char* method, const uint8_t* request, size_t len, cliproxy_buffer* response) {
	if (!bps_host.call) return 1;
	return bps_host.call(bps_host.host_ctx, method, request, len, response);
}
static void bps_free_host(void* ptr, size_t len) {
	if (ptr && bps_host.free_buffer) bps_host.free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"
)

const provider = "codex-bps"
const repository = "https://github.com/zyxzjyzjj/cpa-gptbps-plugin"

var version = "0.1.3"
var runtimePlugin = newPlugin(callHost)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	C.bps_store_host(host)
	runtimePlugin = newPlugin(callHost)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if recover() != nil {
			writeABIResponse(response, failure(&rpcError{Code: "plugin_panic", Message: "codex-bps call failed", HTTPStatus: 500}))
			status = 1
		}
	}()
	if method == nil || length > 512<<20 || (request == nil && length != 0) {
		writeABIResponse(response, failure(badRequest("invalid ABI request")))
		return 1
	}
	raw := C.GoBytes(unsafe.Pointer(request), C.int(length))
	out, err := runtimePlugin.dispatch(C.GoString(method), raw)
	if err != nil {
		writeABIResponse(response, failure(err))
		return 0 // A valid failed envelope is a successful ABI transport.
	}
	writeABIResponse(response, success(out))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { runtimePlugin.close() }

func writeABIResponse(response *C.cliproxy_buffer, raw []byte) {
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

// Host buffers must always be freed by the host, including failed calls.
func callHost(method string, request any, result any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	cmethod := C.CString(method)
	defer C.free(unsafe.Pointer(cmethod))
	data := C.CBytes(raw)
	defer C.free(data)
	var response C.cliproxy_buffer
	rc := C.bps_call_host(cmethod, (*C.uint8_t)(data), C.size_t(len(raw)), &response)
	if response.ptr == nil {
		return errors.New("CPA callback unavailable: " + method)
	}
	defer C.bps_free_host(response.ptr, response.len)
	if response.len > 512<<20 {
		return errors.New("CPA callback response exceeds limit")
	}
	var env envelope
	if json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &env) != nil {
		return errors.New("invalid CPA callback envelope")
	}
	if !env.OK || rc != 0 {
		if env.Error != nil {
			return env.Error
		}
		return errors.New("CPA callback failed: " + method)
	}
	if result != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}
