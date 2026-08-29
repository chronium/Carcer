#include "build.h"

#include "protocol.h"
#include "serial.h"
#include "source_snapshot.h"

#define FRAME_HEADER_SIZE 16u
#define FRAME_PROTOCOL_VERSION 1u
#define HOST_RESPONSE_OUTPUT_MAX (64u * 1024u)

struct response_header {
    uint16_t message_type;
    uint32_t request_id;
    uint32_t payload_length;
};

static const uint8_t build_service_name[] = "build";
static const uint8_t finish_service_name[] = "finish_generation";
static const uint8_t feature_service_name[] = "request_feature";
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

static void send_build_request(
    uint32_t request_id,
    uint16_t selected_count,
    uint32_t snapshot_length
) {
    uint32_t payload_length = 2u + (sizeof(build_service_name) - 1u) + 2u +
                              4u + snapshot_length;

    frame_send_header(HOST_SERVICE_REQUEST, request_id, payload_length);
    frame_write_u16(sizeof(build_service_name) - 1u);
    serial_write_bytes(build_service_name, sizeof(build_service_name) - 1u);
    frame_write_u16(1u);
    frame_write_u32(snapshot_length);
    source_snapshot_write(selected_count);
}

static void send_finish_request(
    uint32_t request_id,
    const uint8_t *handoff,
    uint32_t handoff_length,
    uint16_t selected_count,
    uint32_t snapshot_length
) {
    uint32_t payload_length = 2u + (sizeof(finish_service_name) - 1u) + 2u +
                              4u + handoff_length + 4u + snapshot_length;

    frame_send_header(HOST_SERVICE_REQUEST, request_id, payload_length);
    frame_write_u16(sizeof(finish_service_name) - 1u);
    serial_write_bytes(finish_service_name, sizeof(finish_service_name) - 1u);
    frame_write_u16(2u);
    frame_write_u32(handoff_length);
    serial_write_bytes(handoff, handoff_length);
    frame_write_u32(snapshot_length);
    source_snapshot_write(selected_count);
}

static void send_feature_request(
    uint32_t request_id,
    const uint8_t *title,
    uint32_t title_length,
    const uint8_t *description,
    uint32_t description_length
) {
    uint32_t payload_length = 2u + (sizeof(feature_service_name) - 1u) + 2u +
                              4u + title_length + 4u + description_length;

    frame_send_header(HOST_SERVICE_REQUEST, request_id, payload_length);
    frame_write_u16(sizeof(feature_service_name) - 1u);
    serial_write_bytes(feature_service_name, sizeof(feature_service_name) - 1u);
    frame_write_u16(2u);
    frame_write_u32(title_length);
    serial_write_bytes(title, title_length);
    frame_write_u32(description_length);
    serial_write_bytes(description, description_length);
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

static uint32_t allocate_host_request_id(void) {
    uint32_t request_id = next_host_request_id;
    next_host_request_id = request_id == 0xffffffffu ? 1u : request_id + 1u;
    return request_id;
}

static void relay_host_response(
    uint32_t host_request_id,
    uint32_t tool_request_id
) {
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
    uint32_t output_length = response.payload_length - 4u;
    if (status > 2u || output_length > HOST_RESPONSE_OUTPUT_MAX) {
        discard_bytes(output_length);
        send_tool_failure(tool_request_id);
        return;
    }

    frame_send_header(INVOKE_TOOL_RESPONSE, tool_request_id, 4u + output_length);
    frame_write_u32(status);
    for (uint32_t index = 0; index < output_length; ++index) {
        serial_write(serial_read());
    }
}

void build_tool_invoke(uint32_t tool_request_id) {
    uint16_t selected_count;
    uint32_t snapshot_length;
    uint32_t fixed_payload_length =
        2u + (sizeof(build_service_name) - 1u) + 2u + 4u;

    if (!source_snapshot_measure(&selected_count, &snapshot_length) ||
        snapshot_length > FRAME_MAX_PAYLOAD - fixed_payload_length) {
        send_tool_failure(tool_request_id);
        return;
    }

    uint32_t host_request_id = allocate_host_request_id();
    send_build_request(host_request_id, selected_count, snapshot_length);
    relay_host_response(host_request_id, tool_request_id);
}

void finish_generation_tool_invoke(
    uint32_t tool_request_id,
    const uint8_t *handoff,
    uint32_t handoff_length
) {
    uint16_t selected_count;
    uint32_t snapshot_length;
    uint32_t fixed_payload_length =
        2u + (sizeof(finish_service_name) - 1u) + 2u + 4u + 4u;

    if (handoff_length > FINISH_GENERATION_HANDOFF_MAX ||
        !source_snapshot_measure(&selected_count, &snapshot_length) ||
        fixed_payload_length + handoff_length > FRAME_MAX_PAYLOAD ||
        snapshot_length >
            FRAME_MAX_PAYLOAD - fixed_payload_length - handoff_length) {
        send_tool_failure(tool_request_id);
        return;
    }

    uint32_t host_request_id = allocate_host_request_id();
    send_finish_request(
        host_request_id,
        handoff,
        handoff_length,
        selected_count,
        snapshot_length
    );
    relay_host_response(host_request_id, tool_request_id);
}

void request_feature_tool_invoke(
    uint32_t tool_request_id,
    const uint8_t *title,
    uint32_t title_length,
    const uint8_t *description,
    uint32_t description_length
) {
    uint32_t fixed_payload_length =
        2u + (sizeof(feature_service_name) - 1u) + 2u + 4u + 4u;

    if (title_length == 0u || title_length > FEATURE_REQUEST_TITLE_MAX ||
        description_length > FEATURE_REQUEST_DESCRIPTION_MAX ||
        fixed_payload_length + title_length > FRAME_MAX_PAYLOAD ||
        description_length >
            FRAME_MAX_PAYLOAD - fixed_payload_length - title_length) {
        send_tool_failure(tool_request_id);
        return;
    }

    uint32_t host_request_id = allocate_host_request_id();
    send_feature_request(
        host_request_id,
        title,
        title_length,
        description,
        description_length
    );
    relay_host_response(host_request_id, tool_request_id);
}
