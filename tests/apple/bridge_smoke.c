#include "H264Engine.h"
#include <assert.h>
#include <stdio.h>
#include <string.h>

int main(void) {
    uint8_t key[32] = {0};
    char *error = NULL;
    assert(H264EngineStart("not-json", key, 32, &error) == 0);
    assert(error != NULL);
    H264EngineFree(error); error = NULL;
    uint64_t handle = H264EngineStart("{\"endpoint\":\"127.0.0.1:1\"}", key, 32, &error);
    assert(handle != 0 && error == NULL);
    /* A canceled/incomplete start must remain stoppable through the C ABI. */
    int ready = H264EngineWaitReady(handle, 1, &error);
    assert(ready != 0 && error != NULL);
    H264EngineFree(error); error = NULL;
    H264EngineStop(handle);
    H264EngineStop(handle);
    uint8_t packet[1500] = {0};
    assert(H264EngineRead(handle, packet, sizeof(packet), 1) == -1);
    assert(H264EngineInject(handle, packet, sizeof(packet)) == -1);
    char *status = H264EngineStatus(handle);
    assert(status && strcmp(status, "{\"state\":\"stopped\"}") == 0);
    H264EngineFree(status);
    puts("C bridge: validation, startup cancellation, repeated stop, stale handles passed.");
    return 0;
}
