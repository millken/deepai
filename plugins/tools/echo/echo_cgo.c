#include <stdlib.h>

// plugin_free_string is exported directly from C so that cgo's //export
// machinery doesn't interfere with C library symbol resolution.
void plugin_free_string(void *s) {
	free(s);
}
