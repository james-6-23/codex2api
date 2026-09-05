# API Error Codes Reference

## Standard Error Response Format

All API errors follow this structure:

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable description",
    "type": "error_category",
    "details": {}
  }
}
```

## Error Codes

### Authentication Errors (401)

| Code | Description |
|------|-------------|
| `missing_api_key` | Authorization header is missing |
| `invalid_api_key` | API key format is invalid or key doesn't exist |
| `insufficient_scope` | API key lacks permission for this operation |
| `invalid_auth` | Authentication failed for other reasons |

### Request Validation Errors (400)

| Code | Description |
|------|-------------|
| `invalid_request` | General request format error |
| `invalid_parameter` | A parameter value is invalid |
| `missing_field` | A required field is missing |
| `invalid_field_type` | Field type doesn't match expected type |
| `invalid_field_format` | Field format is invalid |
| `context_length_exceeded` | Total tokens exceed model limit |
| `unsupported_model` | Requested model is not available |

### Rate Limiting Errors (429)

| Code | Description |
|------|-------------|
| `rate_limit_reached` | Request quota exceeded |

### Resource Errors (404, 409)

| Code | Description |
|------|-------------|
| `resource_not_found` | Requested resource doesn't exist |
| `resource_conflict` | Resource conflict occurred |

### Server Errors (500, 503)

| Code | Description |
|------|-------------|
| `server_error` | Internal server error |
| `service_unavailable` | Service temporarily unavailable |
| `no_available_account` | No account is currently available for dispatch |
| `upstream_error` | Error from upstream service |
| `upstream_timeout` | Request to upstream timed out |

Codex Responses, compact Responses, Chat, and Responses WebSocket selection failures now use the generic `service_unavailable` surface with a request ID. Protected NewAPI administrator diagnostics are described in `dispatch-diagnostics.md`; existing quota, session-limit, policy, and upstream error contracts are unchanged.

## Error Types

| Type | Description |
|------|-------------|
| `invalid_request_error` | Request validation or format error |
| `authentication_error` | Authentication failure |
| `permission_error` | Insufficient permissions |
| `not_found_error` | Resource not found |
| `rate_limit_error` | Rate limit exceeded |
| `server_error` | Internal server error |
| `upstream_error` | Upstream service error |

## HTTP Status Codes

| Status | Meaning |
|--------|---------|
| 200 | Success |
| 201 | Created |
| 202 | Accepted |
| 204 | No Content |
| 400 | Bad Request - Validation error |
| 401 | Unauthorized - Authentication error |
| 403 | Forbidden - Permission denied |
| 404 | Not Found |
| 409 | Conflict |
| 429 | Too Many Requests - Rate limited |
| 500 | Internal Server Error |
| 502 | Bad Gateway - Upstream error |
| 503 | Service Unavailable |

## Validation Error Details

When validation fails, the `details` field contains:

```json
{
  "error": {
    "code": "invalid_parameter",
    "message": "Request validation failed",
    "type": "invalid_request_error",
    "details": [
      {
        "field": "temperature",
        "message": "Field 'temperature' must be between 0 and 2",
        "code": "out_of_range"
      },
      {
        "field": "model",
        "message": "Model 'invalid-model' is not supported",
        "code": "unsupported_model"
      }
    ]
  }
}
```

## Validation Rule Codes

| Code | Description |
|------|-------------|
| `required` | Field is required |
| `null_not_allowed` | Field cannot be null |
| `empty_not_allowed` | Field cannot be empty |
| `type_error` | Field type is incorrect |
| `min_length` | String too short |
| `max_length` | String too long |
| `min_value` | Number too small |
| `max_value` | Number too large |
| `out_of_range` | Value out of allowed range |
| `invalid_enum_value` | Value not in allowed enum |
| `pattern_mismatch` | String doesn't match pattern |
| `min_items` | Array too small |
| `max_items` | Array too large |
| `missing_message_role` | Message missing role field |
| `invalid_message_role` | Invalid message role |
| `missing_message_content` | Message missing content field |
| `empty_input` | Input array empty |
| `missing_input_type` | Input item missing type field |
| `invalid_input_type` | Invalid input type |

## Backward Compatibility

For backward compatibility, legacy error formats are supported but new integrations
should use the standardized format above.
