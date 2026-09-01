#ifndef OPENCODEX_RELAY_LEGACY_KEYCHAIN_ACL_H
#define OPENCODEX_RELAY_LEGACY_KEYCHAIN_ACL_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

enum {
    OCRLegacyKeychainResultSuccess = 0,
    OCRLegacyKeychainResultFailure = 1,
    OCRLegacyKeychainResultTrustedApplicationUnavailable = 2,
    OCRLegacyKeychainResultInvalidInput = 3,
};

typedef struct {
    bool configured;
    bool has_modification_time;
    double modification_time_since_reference_date;
} OCRLegacyKeychainItemMetadata;

int32_t OCRLegacyKeychainCopyItemMetadata(
    const char *service,
    const char *account,
    const char *expected_keychain_path,
    OCRLegacyKeychainItemMetadata *metadata
);

int32_t OCRLegacyKeychainInspectDecryptACL(
    const char *service,
    const char *account,
    const char *application_path,
    const char *security_tool_path,
    char *keychain_path_buffer,
    size_t keychain_path_buffer_capacity,
    bool *matches
);

#ifdef __cplusplus
}
#endif

#endif
