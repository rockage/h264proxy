#ifndef H264_ENGINE_H
#define H264_ENGINE_H
#include <stdint.h>
#include <stddef.h>
#ifdef __cplusplus
extern "C" {
#endif
/* All packet inputs are copied. WaitReady/Read/Stop may block; call off the UI queue.
   Returned strings are engine-owned allocations; release with H264EngineFree. */
uint64_t H264EngineStart(const char *configuration, const uint8_t *key, int keyLength, char **errorOut);
int H264EngineWaitReady(uint64_t handle, int timeoutMillis, char **errorOut);
int H264EngineInject(uint64_t handle, const void *packet, int length);
int H264EngineRead(uint64_t handle, void *buffer, int capacity, int timeoutMillis);
char *H264EngineStatus(uint64_t handle);
void H264EngineStop(uint64_t handle);
void H264EngineFree(void *pointer);
#ifdef __cplusplus
}
#endif
#endif
