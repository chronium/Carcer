#include "tools.h"
#include "build.h"
#include "files.h"
#include "protocol.h"
#include "serial.h"
#define MAX_TOOL_ARGUMENTS 3u
#define TOOL_SUCCESS 0u
#define TOOL_FAILURE 1u
struct bytes {
const uint8_t *data;
uint32_t length;
};
struct invocation {
struct bytes name;
uint16_t argument_count;
struct bytes arguments[MAX_TOOL_ARGUMENTS];
};
static int bytes_equal(
const uint8_t *left,
uint32_t left_length,
const uint8_t *right,
uint32_t right_length
) {
if (left_length != right_length) {
return 0;
}
for (uint32_t index = 0; index < left_length; ++index) {
if (left[index] != right[index]) {
return 0;
}
}
return 1;
}
static uint16_t read_u16_le(const uint8_t *bytes) {
return (uint16_t)bytes[0] | ((uint16_t)bytes[1] << 8);
}
static uint32_t read_u32_le(const uint8_t *bytes) {
return (uint32_t)bytes[0] | ((uint32_t)bytes[1] << 8) |
((uint32_t)bytes[2] << 16) | ((uint32_t)bytes[3] << 24);
}
static int valid_utf8(const uint8_t *data, uint32_t length) {
uint32_t index = 0;
while (index < length) {
uint8_t first = data[index++];
uint32_t codepoint;
uint32_t minimum;
uint32_t remaining;
if (first <= 0x7f) {
continue;
}
if (first >= 0xc2 && first <= 0xdf) {
codepoint = first & 0x1f;
minimum = 0x80;
remaining = 1;
} else if (first >= 0xe0 && first <= 0xef) {
codepoint = first & 0x0f;
minimum = 0x800;
remaining = 2;
} else if (first >= 0xf0 && first <= 0xf4) {
codepoint = first & 0x07;
minimum = 0x10000;
remaining = 3;
} else {
return 0;
}
if (remaining > length - index) {
return 0;
}
for (uint32_t count = 0; count < remaining; ++count) {
uint8_t next = data[index++];
if ((next & 0xc0) != 0x80) {
return 0;
}
codepoint = (codepoint << 6) | (next & 0x3f);
}
if (codepoint < minimum || codepoint > 0x10ffff ||
(codepoint >= 0xd800 && codepoint <= 0xdfff)) {
return 0;
}
}
return 1;
}
static int parse_decimal(struct bytes argument, uint32_t *value) {
uint32_t parsed = 0;
if (argument.length == 0) {
return 0;
}
for (uint32_t index = 0; index < argument.length; ++index) {
uint8_t byte = argument.data[index];
if (byte < '0' || byte > '9') {
return 0;
}
uint32_t digit = byte - '0';
if (parsed > (0xffffffffu - digit) / 10u) {
return 0;
}
parsed = parsed * 10u + digit;
}
*value = parsed;
return 1;
}
static int valid_path(struct bytes path) {
return path.length != 0 && path.length <= FILE_MAX_PATH_LENGTH &&
valid_utf8(path.data, path.length);
}
static void send_invoke_header(
uint32_t request_id,
uint32_t status,
uint32_t output_length
) {
frame_send_header(INVOKE_TOOL_RESPONSE, request_id, 4u + output_length);
frame_write_u32(status);
}
void tools_send_failure(uint32_t request_id) {
send_invoke_header(request_id, TOOL_FAILURE, 0);
}
static void send_tool_success(uint32_t request_id) {
send_invoke_header(request_id, TOOL_SUCCESS, 0);
}
void tools_send_list(uint32_t request_id) {
static const uint8_t list_name[] = "list";
static const uint8_t read_name[] = "read";
static const uint8_t write_name[] = "write";
static const uint8_t truncate_name[] = "truncate";
static const uint8_t remove_name[] = "remove";
static const uint8_t build_name[] = "build";
static const uint8_t finish_generation_name[] = "finish_generation";
static const uint8_t request_feature_name[] = "request_feature";
uint32_t payload_length = 2u + 2u + (sizeof(list_name) - 1u) + 2u +
(sizeof(read_name) - 1u) + 2u +
(sizeof(write_name) - 1u) + 2u +
(sizeof(truncate_name) - 1u) + 2u +
(sizeof(remove_name) - 1u) + 2u +
(sizeof(build_name) - 1u) + 2u +
(sizeof(finish_generation_name) - 1u) + 2u +
(sizeof(request_feature_name) - 1u);
frame_send_header(LIST_TOOLS_RESPONSE, request_id, payload_length);
frame_write_u16(8);
frame_write_u16(sizeof(list_name) - 1u);
serial_write_bytes(list_name, sizeof(list_name) - 1u);
frame_write_u16(sizeof(read_name) - 1u);
serial_write_bytes(read_name, sizeof(read_name) - 1u);
frame_write_u16(sizeof(write_name) - 1u);
serial_write_bytes(write_name, sizeof(write_name) - 1u);
frame_write_u16(sizeof(truncate_name) - 1u);
serial_write_bytes(truncate_name, sizeof(truncate_name) - 1u);
frame_write_u16(sizeof(remove_name) - 1u);
serial_write_bytes(remove_name, sizeof(remove_name) - 1u);
frame_write_u16(sizeof(build_name) - 1u);
serial_write_bytes(build_name, sizeof(build_name) - 1u);
frame_write_u16(sizeof(finish_generation_name) - 1u);
serial_write_bytes(
finish_generation_name,
sizeof(finish_generation_name) - 1u
);
frame_write_u16(sizeof(request_feature_name) - 1u);
serial_write_bytes(
request_feature_name,
sizeof(request_feature_name) - 1u
);
}
static int parse_invocation(
const uint8_t *payload,
uint32_t payload_length,
struct invocation *invocation
) {
uint32_t offset = 0;
if (payload_length < 2) {
return 0;
}
uint16_t name_length = read_u16_le(payload);
offset += 2;
if (name_length == 0 || name_length > 255 ||
name_length > payload_length - offset) {
return 0;
}
invocation->name.data = payload + offset;
invocation->name.length = name_length;
if (!valid_utf8(invocation->name.data, invocation->name.length)) {
return 0;
}
offset += name_length;
if (payload_length - offset < 2) {
return 0;
}
invocation->argument_count = read_u16_le(payload + offset);
offset += 2;
if (invocation->argument_count > MAX_TOOL_ARGUMENTS) {
return 0;
}
for (uint16_t index = 0; index < invocation->argument_count; ++index) {
if (payload_length - offset < 4) {
return 0;
}
uint32_t argument_length = read_u32_le(payload + offset);
offset += 4;
if (argument_length > payload_length - offset) {
return 0;
}
invocation->arguments[index].data = payload + offset;
invocation->arguments[index].length = argument_length;
offset += argument_length;
}
return offset == payload_length;
}
static void invoke_list(uint32_t request_id, const struct invocation *invocation) {
struct bytes prefix = {(const uint8_t *)0, 0};
if (invocation->argument_count > 1) {
tools_send_failure(request_id);
return;
}
if (invocation->argument_count == 1) {
prefix = invocation->arguments[0];
if (!valid_utf8(prefix.data, prefix.length)) {
tools_send_failure(request_id);
return;
}
}
uint32_t output_length = 0;
for (uint32_t index = 0; index < file_count; ++index) {
if (file_path_has_prefix(&files[index], prefix.data, prefix.length)) {
output_length += files[index].path_length + 1u;
}
}
send_invoke_header(request_id, TOOL_SUCCESS, output_length);
for (uint32_t index = 0; index < file_count; ++index) {
if (file_path_has_prefix(&files[index], prefix.data, prefix.length)) {
serial_write_bytes(files[index].path, files[index].path_length);
serial_write('\n');
}
}
}
static void invoke_read(uint32_t request_id, const struct invocation *invocation) {
uint32_t offset;
uint32_t requested_length;
if (invocation->argument_count != 3 ||
!valid_path(invocation->arguments[0]) ||
!parse_decimal(invocation->arguments[1], &offset) ||
!parse_decimal(invocation->arguments[2], &requested_length) ||
requested_length > FRAME_MAX_PAYLOAD - 4u) {
tools_send_failure(request_id);
return;
}
struct file *file = file_find(
invocation->arguments[0].data,
invocation->arguments[0].length
);
if (file == (struct file *)0) {
tools_send_failure(request_id);
return;
}
uint32_t size = file_size(file);
if (offset > size) {
tools_send_failure(request_id);
return;
}
uint32_t available = size - offset;
uint32_t output_length =
requested_length < available ? requested_length : available;
send_invoke_header(request_id, TOOL_SUCCESS, output_length);
serial_write_bytes(file_content(file) + offset, output_length);
}
static void invoke_write(uint32_t request_id, const struct invocation *invocation) {
uint32_t offset;
if (invocation->argument_count != 3 ||
!valid_path(invocation->arguments[0]) ||
!parse_decimal(invocation->arguments[1], &offset) ||
!file_write(
invocation->arguments[0].data,
invocation->arguments[0].length,
offset,
invocation->arguments[2].data,
invocation->arguments[2].length
)) {
tools_send_failure(request_id);
return;
}
send_tool_success(request_id);
}
static void invoke_truncate(
uint32_t request_id,
const struct invocation *invocation
) {
uint32_t size;
if (invocation->argument_count != 2 ||
!valid_path(invocation->arguments[0]) ||
!parse_decimal(invocation->arguments[1], &size) ||
!file_truncate(
invocation->arguments[0].data,
invocation->arguments[0].length,
size
)) {
tools_send_failure(request_id);
return;
}
send_tool_success(request_id);
}
static void invoke_remove(uint32_t request_id, const struct invocation *invocation) {
if (invocation->argument_count != 1 ||
!valid_path(invocation->arguments[0]) ||
!file_remove(
invocation->arguments[0].data,
invocation->arguments[0].length
)) {
tools_send_failure(request_id);
return;
}
send_tool_success(request_id);
}
static void invoke_build(uint32_t request_id, const struct invocation *invocation) {
if (invocation->argument_count != 0) {
tools_send_failure(request_id);
return;
}
build_tool_invoke(request_id);
}
static void invoke_finish_generation(
uint32_t request_id,
const struct invocation *invocation
) {
if (invocation->argument_count != 1 ||
invocation->arguments[0].length > FINISH_GENERATION_HANDOFF_MAX ||
!valid_utf8(
invocation->arguments[0].data,
invocation->arguments[0].length
)) {
tools_send_failure(request_id);
return;
}
finish_generation_tool_invoke(
request_id,
invocation->arguments[0].data,
invocation->arguments[0].length
);
}
static void invoke_request_feature(
uint32_t request_id,
const struct invocation *invocation
) {
if (invocation->argument_count != 2 ||
invocation->arguments[0].length == 0u ||
invocation->arguments[0].length > FEATURE_REQUEST_TITLE_MAX ||
invocation->arguments[1].length > FEATURE_REQUEST_DESCRIPTION_MAX ||
!valid_utf8(
invocation->arguments[0].data,
invocation->arguments[0].length
) ||
!valid_utf8(
invocation->arguments[1].data,
invocation->arguments[1].length
)) {
tools_send_failure(request_id);
return;
}
request_feature_tool_invoke(
request_id,
invocation->arguments[0].data,
invocation->arguments[0].length,
invocation->arguments[1].data,
invocation->arguments[1].length
);
}
void tools_handle_invocation(
uint32_t request_id,
const uint8_t *payload,
uint32_t payload_length
) {
static const uint8_t list_name[] = "list";
static const uint8_t read_name[] = "read";
static const uint8_t write_name[] = "write";
static const uint8_t truncate_name[] = "truncate";
static const uint8_t remove_name[] = "remove";
static const uint8_t build_name[] = "build";
static const uint8_t finish_generation_name[] = "finish_generation";
static const uint8_t request_feature_name[] = "request_feature";
struct invocation invocation;
if (!parse_invocation(payload, payload_length, &invocation)) {
tools_send_failure(request_id);
return;
}
if (bytes_equal(
invocation.name.data,
invocation.name.length,
list_name,
sizeof(list_name) - 1u
)) {
invoke_list(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
read_name,
sizeof(read_name) - 1u
)) {
invoke_read(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
write_name,
sizeof(write_name) - 1u
)) {
invoke_write(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
truncate_name,
sizeof(truncate_name) - 1u
)) {
invoke_truncate(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
remove_name,
sizeof(remove_name) - 1u
)) {
invoke_remove(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
build_name,
sizeof(build_name) - 1u
)) {
invoke_build(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
finish_generation_name,
sizeof(finish_generation_name) - 1u
)) {
invoke_finish_generation(request_id, &invocation);
} else if (bytes_equal(
invocation.name.data,
invocation.name.length,
request_feature_name,
sizeof(request_feature_name) - 1u
)) {
invoke_request_feature(request_id, &invocation);
} else {
tools_send_failure(request_id);
}
}
