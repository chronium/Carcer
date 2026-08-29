#include "build.h"

#include "files.h"
#include "protocol.h"
#include "serial.h"

#define FRAME_HEADER_SIZE 16u
#define FRAME_PROTOCOL_VERSION 1u
#define BUILD_DIAGNOSTIC_MAX (64u * 1024u)

struct response_header {
    uint16_t message_type;
    uint32_t request_id;
    uint32_t payload_length;
};

static uint32_t next_host_request_id = 1u;

static uint16_t read_u16_le(const uint8_t *bytes) {
    return (uint16_t)bytes[0] | ((uint16_t)bytes[1] << 8);
}

static uint32_t read_u32_le(const uint8_t *bytes) {
    return (uint32_t)bytes[0] | ((uint32_t)bytes[1] << 8) |
           ((uint32_t)bytes[2] << 16) | ((uint32_t)bytes[3] << 24);
}

__attribute__((noreturn)) static void protocol_fatal(void) {
    for (;;) {
        __asm__ volatile("pause");
    }
}

static void discard_bytes(uint32_t count) {
    for (uint32_t index = 0; index < count; ++index) {
        (void)serial_read();
    }
}

static int build_path_valid(const struct file *file) {
    static const uint8_t prefix[] = "seed/";

    if (file->path_length <= sizeof(prefix) - 1u) {
        return 0;
    }
    for (uint32_t index = 0; index < sizeof(prefix) - 1u; ++index) {
        if (file->path[index] != prefix[index]) {
            return 0;
        }
    }
    for (uint32_t index = 0; index < file->path_length; ++index) {
        if (file->path[index] == 0) {
            return 0;
        }
    }

    uint32_t component_start = sizeof(prefix) - 1u;
    for (uint32_t index = component_start; index <= file->path_length; ++index) {
        if (index != file->path_length && file->path[index] != '/') {
            continue;
        }
        uint32_t component_length = index - component_start;
        if (component_length == 0 ||
            (component_length == 1 && file->path[component_start] == '.') ||
            (component_length == 2 && file->path[component_start] == '.' &&
             file->path[component_start + 1u] == '.')) {
            return 0;
        }
        component_start = index + 1u;
    }
    return 1;
}

static int measure_snapshot(uint16_t *selected_count, uint32_t *snapshot_length) {
    uint16_t count = 0;
    uint32_t length = 2u;

    for (uint32_t index = 0; index < file_count; ++index) {
        const struct file *file = &files[index];
        if (!build_path_valid(file)) {
            continue;
        }
        uint32_t entry_length = 2u + file->path_length + 4u + file_size(file);
        if (entry_length > FRAME_MAX_PAYLOAD - length) {
            return 0;
        }
        length += entry_length;
        ++count;
    }
    *selected_count = count;
    *snapshot_length = length;
    return 1;
}

static void send_build_request(
    uint32_t request_id,
    uint16_t selected_count,
    uint32_t snapshot_length
) {
    static const uint8_t service_name[] = "build";
    uint32_t payload_length = 2u + (sizeof(service_name) - 1u) + 2u + 4u +
                              snapshot_length;

    frame_send_header(HOST_SERVICE_REQUEST, request_id, payload_length);
    frame_write_u16(sizeof(service_name) - 1u);
    serial_write_bytes(service_name, sizeof(service_name) - 1u);
    frame_write_u16(1u);
    frame_write_u32(snapshot_length);
    frame_write_u16(selected_count);

    for (uint32_t index = 0; index < file_count; ++index) {
        const struct file *file = &files[index];
        if (!build_path_valid(file)) {
            continue;
        }
        frame_write_u16(file->path_length);
        serial_write_bytes(file->path, file->path_length);
        frame_write_u32(file_size(file));
        serial_write_bytes(file_content(file), file_size(file));
    }
}

static void read_response_header(struct response_header *response) {
    uint8_t header[FRAME_HEADER_SIZE];

    for (uint32_t index = 0; index < sizeof(header); ++index) {
        header[index] = serial_read();
    }
    response->message_type = read_u16_le(header + 6);
    response->request_id = read_u32_le(header + 8);
    response->payload_length = read_u32_le(header + 12);

    if (response->payload_length > FRAME_MAX_PAYLOAD || header[0] != 'C' ||
        header[1] != 'X' || header[2] != 'O' || header[3] != 'S' ||
        read_u16_le(header + 4) != FRAME_PROTOCOL_VERSION) {
        protocol_fatal();
    }
}

static void send_tool_failure(uint32_t tool_request_id) {
    frame_send_header(INVOKE_TOOL_RESPONSE, tool_request_id, 4u);
    frame_write_u32(1u);
}

void build_tool_invoke(uint32_t tool_request_id) {
    uint16_t selected_count;
    uint32_t snapshot_length;

    if (!measure_snapshot(&selected_count, &snapshot_length) ||
        snapshot_length > FRAME_MAX_PAYLOAD - 13u) {
        send_tool_failure(tool_request_id);
        return;
    }

    uint32_t host_request_id = next_host_request_id;
    next_host_request_id =
        host_request_id == 0xffffffffu ? 1u : host_request_id + 1u;
    send_build_request(host_request_id, selected_count, snapshot_length);

    struct response_header response;
    read_response_header(&response);
    if (response.message_type != HOST_SERVICE_RESPONSE ||
        response.request_id != host_request_id || response.payload_length < 4u) {
        discard_bytes(response.payload_length);
        send_tool_failure(tool_request_id);
        return;
    }

    uint8_t status_bytes[4];
    for (uint32_t index = 0; index < sizeof(status_bytes); ++index) {
        status_bytes[index] = serial_read();
    }
    uint32_t status = read_u32_le(status_bytes);
    uint32_t diagnostics_length = response.payload_length - 4u;
    if (status > 2u || diagnostics_length > BUILD_DIAGNOSTIC_MAX) {
        discard_bytes(diagnostics_length);
        send_tool_failure(tool_request_id);
        return;
    }

    frame_send_header(
        INVOKE_TOOL_RESPONSE,
        tool_request_id,
        4u + diagnostics_length
    );
    frame_write_u32(status);
    for (uint32_t index = 0; index < diagnostics_length; ++index) {
        serial_write(serial_read());
    }
}
