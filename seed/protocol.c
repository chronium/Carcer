#include "protocol.h"
#include "serial.h"
#include "tools.h"
#define FRAME_HEADER_SIZE 16u
#define FRAME_PROTOCOL_VERSION 1u
#define FINISH_GENERATION_INVOCATION_OVERHEAD 25u
#define FEATURE_REQUEST_INVOCATION_OVERHEAD 27u
#define REQUEST_BUFFER_SIZE \
(16u * 1024u + 256u + FEATURE_REQUEST_INVOCATION_OVERHEAD)
static uint16_t read_u16_le(const uint8_t *bytes) {
return (uint16_t)bytes[0] | ((uint16_t)bytes[1] << 8);
}
static uint32_t read_u32_le(const uint8_t *bytes) {
return (uint32_t)bytes[0] | ((uint32_t)bytes[1] << 8) |
((uint32_t)bytes[2] << 16) | ((uint32_t)bytes[3] << 24);
}
static void write_u16_le(uint8_t *bytes, uint16_t value) {
bytes[0] = (uint8_t)value;
bytes[1] = (uint8_t)(value >> 8);
}
static void write_u32_le(uint8_t *bytes, uint32_t value) {
bytes[0] = (uint8_t)value;
bytes[1] = (uint8_t)(value >> 8);
bytes[2] = (uint8_t)(value >> 16);
bytes[3] = (uint8_t)(value >> 24);
}
static void discard_bytes(uint32_t count) {
for (uint32_t index = 0; index < count; ++index) {
(void)serial_read();
}
}
void frame_send_header(
uint16_t message_type,
uint32_t request_id,
uint32_t payload_length
) {
uint8_t header[FRAME_HEADER_SIZE];
header[0] = 'C';
header[1] = 'X';
header[2] = 'O';
header[3] = 'S';
write_u16_le(header + 4, FRAME_PROTOCOL_VERSION);
write_u16_le(header + 6, message_type);
write_u32_le(header + 8, request_id);
write_u32_le(header + 12, payload_length);
serial_write_bytes(header, sizeof(header));
}
void frame_write_u16(uint16_t value) {
uint8_t bytes[2];
write_u16_le(bytes, value);
serial_write_bytes(bytes, sizeof(bytes));
}
void frame_write_u32(uint32_t value) {
uint8_t bytes[4];
write_u32_le(bytes, value);
serial_write_bytes(bytes, sizeof(bytes));
}
__attribute__((noreturn)) void protocol_loop(void) {
uint8_t header[FRAME_HEADER_SIZE];
uint8_t payload[REQUEST_BUFFER_SIZE];
for (;;) {
for (uint32_t index = 0; index < sizeof(header); ++index) {
header[index] = serial_read();
}
uint16_t version = read_u16_le(header + 4);
uint16_t message_type = read_u16_le(header + 6);
uint32_t request_id = read_u32_le(header + 8);
uint32_t payload_length = read_u32_le(header + 12);
if (payload_length > FRAME_MAX_PAYLOAD) {
for (;;) {
__asm__ volatile("pause");
}
}
int valid_header = header[0] == 'C' && header[1] == 'X' &&
header[2] == 'O' && header[3] == 'S' &&
version == FRAME_PROTOCOL_VERSION;
if (payload_length > sizeof(payload)) {
discard_bytes(payload_length);
if (valid_header && message_type == INVOKE_TOOL_REQUEST &&
request_id != 0) {
tools_send_failure(request_id);
}
continue;
}
for (uint32_t index = 0; index < payload_length; ++index) {
payload[index] = serial_read();
}
if (!valid_header || request_id == 0) {
continue;
}
if (message_type == LIST_TOOLS_REQUEST && payload_length == 0) {
tools_send_list(request_id);
} else if (message_type == INVOKE_TOOL_REQUEST) {
tools_handle_invocation(request_id, payload, payload_length);
}
}
}
