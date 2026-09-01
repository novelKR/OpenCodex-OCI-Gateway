#include "OpenCodexRelayLegacyKeychainACL.h"

#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <limits.h>
#include <string.h>

static int32_t copy_user_default_keychain(SecKeychainRef *keychain) {
    if (keychain == NULL) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    *keychain = NULL;
    if (SecKeychainCopyDomainDefault(kSecPreferencesDomainUser, keychain) != errSecSuccess ||
        *keychain == NULL) {
        return OCRLegacyKeychainResultFailure;
    }
    return OCRLegacyKeychainResultSuccess;
}

static CFStringRef copy_utf8_string(const char *value) {
    if (value == NULL || value[0] == '\0') {
        return NULL;
    }
    return CFStringCreateWithCString(
        kCFAllocatorDefault,
        value,
        kCFStringEncodingUTF8
    );
}

static int32_t copy_keychain_path(
    SecKeychainRef keychain,
    char *path_buffer,
    size_t path_buffer_capacity
) {
    if (keychain == NULL || path_buffer == NULL || path_buffer_capacity == 0 ||
        path_buffer_capacity > UINT32_MAX) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    path_buffer[0] = '\0';
    UInt32 length = (UInt32)(path_buffer_capacity - 1);
    OSStatus status = SecKeychainGetPath(keychain, &length, path_buffer);
    if (status != errSecSuccess || length == 0 || length >= path_buffer_capacity) {
        path_buffer[0] = '\0';
        return OCRLegacyKeychainResultFailure;
    }
    path_buffer[length] = '\0';
    return OCRLegacyKeychainResultSuccess;
}

static CFMutableDictionaryRef copy_item_query(
    SecKeychainRef keychain,
    CFStringRef service,
    CFStringRef account
) {
    const void *search_values[] = {keychain};
    CFArrayRef search_list = CFArrayCreate(
        kCFAllocatorDefault,
        search_values,
        1,
        &kCFTypeArrayCallBacks
    );
    if (search_list == NULL) {
        return NULL;
    }

    CFMutableDictionaryRef query = CFDictionaryCreateMutable(
        kCFAllocatorDefault,
        0,
        &kCFTypeDictionaryKeyCallBacks,
        &kCFTypeDictionaryValueCallBacks
    );
    if (query != NULL) {
        CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword);
        CFDictionarySetValue(query, kSecAttrService, service);
        CFDictionarySetValue(query, kSecAttrAccount, account);
        CFDictionarySetValue(query, kSecMatchSearchList, search_list);
        CFDictionarySetValue(query, kSecMatchLimit, kSecMatchLimitOne);
    }
    CFRelease(search_list);
    return query;
}

static int32_t copy_item(
    SecKeychainRef keychain,
    CFStringRef service,
    CFStringRef account,
    SecKeychainItemRef *item,
    bool *found
) {
    if (item == NULL || found == NULL) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    *item = NULL;
    *found = false;
    CFMutableDictionaryRef query = copy_item_query(keychain, service, account);
    if (query == NULL) {
        return OCRLegacyKeychainResultFailure;
    }
    CFDictionarySetValue(query, kSecReturnRef, kCFBooleanTrue);

    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(query, &result);
    CFRelease(query);
    if (status == errSecItemNotFound) {
        return OCRLegacyKeychainResultSuccess;
    }
    if (status != errSecSuccess || result == NULL ||
        CFGetTypeID(result) != SecKeychainItemGetTypeID()) {
        if (result != NULL) {
            CFRelease(result);
        }
        return OCRLegacyKeychainResultFailure;
    }

    *item = (SecKeychainItemRef)result;
    *found = true;
    return OCRLegacyKeychainResultSuccess;
}

static int32_t append_trusted_application_identities(
    CFArrayRef applications,
    CFMutableArrayRef identities
) {
    CFIndex count = CFArrayGetCount(applications);
    for (CFIndex index = 0; index < count; index += 1) {
        SecTrustedApplicationRef application =
            (SecTrustedApplicationRef)CFArrayGetValueAtIndex(applications, index);
        CFDataRef identity = NULL;
        if (application == NULL ||
            SecTrustedApplicationCopyData(application, &identity) != errSecSuccess ||
            identity == NULL) {
            if (identity != NULL) {
                CFRelease(identity);
            }
            return OCRLegacyKeychainResultFailure;
        }
        CFArrayAppendValue(identities, identity);
        CFRelease(identity);
    }
    return OCRLegacyKeychainResultSuccess;
}

static int32_t copy_trusted_application_identity(
    const char *path,
    CFDataRef *identity
) {
    if (path == NULL || path[0] == '\0' || identity == NULL) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    *identity = NULL;
    SecTrustedApplicationRef application = NULL;
    if (SecTrustedApplicationCreateFromPath(path, &application) != errSecSuccess ||
        application == NULL) {
        return OCRLegacyKeychainResultTrustedApplicationUnavailable;
    }
    OSStatus status = SecTrustedApplicationCopyData(application, identity);
    CFRelease(application);
    if (status != errSecSuccess || *identity == NULL) {
        if (*identity != NULL) {
            CFRelease(*identity);
            *identity = NULL;
        }
        return OCRLegacyKeychainResultTrustedApplicationUnavailable;
    }
    return OCRLegacyKeychainResultSuccess;
}

int32_t OCRLegacyKeychainCopyItemMetadata(
    const char *service_value,
    const char *account_value,
    const char *expected_keychain_path,
    OCRLegacyKeychainItemMetadata *metadata
) {
    if (metadata == NULL) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    metadata->configured = false;
    metadata->has_modification_time = false;
    metadata->modification_time_since_reference_date = 0;

    CFStringRef service = copy_utf8_string(service_value);
    CFStringRef account = copy_utf8_string(account_value);
    if (service == NULL || account == NULL) {
        if (service != NULL) {
            CFRelease(service);
        }
        if (account != NULL) {
            CFRelease(account);
        }
        return OCRLegacyKeychainResultInvalidInput;
    }

    SecKeychainRef keychain = NULL;
    int32_t result = copy_user_default_keychain(&keychain);
    if (result != OCRLegacyKeychainResultSuccess) {
        CFRelease(service);
        CFRelease(account);
        return result;
    }
    if (expected_keychain_path != NULL) {
        char actual_keychain_path[PATH_MAX + 1];
        result = copy_keychain_path(
            keychain,
            actual_keychain_path,
            sizeof(actual_keychain_path)
        );
        if (result != OCRLegacyKeychainResultSuccess ||
            strcmp(actual_keychain_path, expected_keychain_path) != 0) {
            CFRelease(keychain);
            CFRelease(service);
            CFRelease(account);
            return OCRLegacyKeychainResultFailure;
        }
    }
    CFMutableDictionaryRef query = copy_item_query(keychain, service, account);
    CFRelease(keychain);
    CFRelease(service);
    CFRelease(account);
    if (query == NULL) {
        return OCRLegacyKeychainResultFailure;
    }
    CFDictionarySetValue(query, kSecReturnAttributes, kCFBooleanTrue);

    CFTypeRef item_attributes = NULL;
    OSStatus status = SecItemCopyMatching(query, &item_attributes);
    CFRelease(query);
    if (status == errSecItemNotFound) {
        return OCRLegacyKeychainResultSuccess;
    }
    if (status != errSecSuccess || item_attributes == NULL ||
        CFGetTypeID(item_attributes) != CFDictionaryGetTypeID()) {
        if (item_attributes != NULL) {
            CFRelease(item_attributes);
        }
        return OCRLegacyKeychainResultFailure;
    }

    metadata->configured = true;
    CFTypeRef modified = CFDictionaryGetValue(
        (CFDictionaryRef)item_attributes,
        kSecAttrModificationDate
    );
    if (modified != NULL && CFGetTypeID(modified) == CFDateGetTypeID()) {
        metadata->has_modification_time = true;
        metadata->modification_time_since_reference_date =
            CFDateGetAbsoluteTime((CFDateRef)modified);
    }
    CFRelease(item_attributes);
    return OCRLegacyKeychainResultSuccess;
}

int32_t OCRLegacyKeychainInspectDecryptACL(
    const char *service_value,
    const char *account_value,
    const char *application_path,
    const char *security_tool_path,
    char *keychain_path_buffer,
    size_t keychain_path_buffer_capacity,
    bool *matches
) {
    if (matches == NULL || keychain_path_buffer == NULL ||
        keychain_path_buffer_capacity == 0) {
        return OCRLegacyKeychainResultInvalidInput;
    }
    *matches = false;
    keychain_path_buffer[0] = '\0';
    CFStringRef service = copy_utf8_string(service_value);
    CFStringRef account = copy_utf8_string(account_value);
    if (service == NULL || account == NULL) {
        if (service != NULL) {
            CFRelease(service);
        }
        if (account != NULL) {
            CFRelease(account);
        }
        return OCRLegacyKeychainResultInvalidInput;
    }

    SecKeychainRef keychain = NULL;
    int32_t result = copy_user_default_keychain(&keychain);
    if (result != OCRLegacyKeychainResultSuccess) {
        CFRelease(service);
        CFRelease(account);
        return result;
    }
    result = copy_keychain_path(
        keychain,
        keychain_path_buffer,
        keychain_path_buffer_capacity
    );
    if (result != OCRLegacyKeychainResultSuccess) {
        CFRelease(keychain);
        CFRelease(service);
        CFRelease(account);
        return result;
    }
    SecKeychainItemRef item = NULL;
    bool found = false;
    result = copy_item(keychain, service, account, &item, &found);
    CFRelease(keychain);
    CFRelease(service);
    CFRelease(account);
    if (result != OCRLegacyKeychainResultSuccess || !found) {
        return result;
    }

    SecAccessRef access = NULL;
    if (SecKeychainItemCopyAccess(item, &access) != errSecSuccess || access == NULL) {
        CFRelease(item);
        if (access != NULL) {
            CFRelease(access);
        }
        return OCRLegacyKeychainResultFailure;
    }
    CFRelease(item);

    CFArrayRef acl_list = SecAccessCopyMatchingACLList(
        access,
        kSecACLAuthorizationDecrypt
    );
    CFRelease(access);
    if (acl_list == NULL || CFArrayGetCount(acl_list) == 0) {
        if (acl_list != NULL) {
            CFRelease(acl_list);
        }
        return OCRLegacyKeychainResultSuccess;
    }

    CFMutableArrayRef actual_identities = CFArrayCreateMutable(
        kCFAllocatorDefault,
        0,
        &kCFTypeArrayCallBacks
    );
    if (actual_identities == NULL) {
        CFRelease(acl_list);
        return OCRLegacyKeychainResultFailure;
    }
    CFIndex acl_count = CFArrayGetCount(acl_list);
    for (CFIndex index = 0; index < acl_count; index += 1) {
        SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acl_list, index);
        CFArrayRef applications = NULL;
        CFStringRef description = NULL;
        SecKeychainPromptSelector prompt_selector = 0;
        OSStatus status = SecACLCopyContents(
            acl,
            &applications,
            &description,
            &prompt_selector
        );
        if (description != NULL) {
            CFRelease(description);
        }
        if (status != errSecSuccess || applications == NULL) {
            if (applications != NULL) {
                CFRelease(applications);
            }
            CFRelease(actual_identities);
            CFRelease(acl_list);
            return status == errSecSuccess
                ? OCRLegacyKeychainResultSuccess
                : OCRLegacyKeychainResultFailure;
        }
        result = append_trusted_application_identities(
            applications,
            actual_identities
        );
        CFRelease(applications);
        if (result != OCRLegacyKeychainResultSuccess) {
            CFRelease(actual_identities);
            CFRelease(acl_list);
            return result;
        }
    }
    CFRelease(acl_list);

    CFDataRef application_identity = NULL;
    CFDataRef security_tool_identity = NULL;
    result = copy_trusted_application_identity(
        application_path,
        &application_identity
    );
    if (result == OCRLegacyKeychainResultSuccess) {
        result = copy_trusted_application_identity(
            security_tool_path,
            &security_tool_identity
        );
    }
    if (result == OCRLegacyKeychainResultSuccess) {
        CFRange range = CFRangeMake(0, CFArrayGetCount(actual_identities));
        *matches = CFArrayContainsValue(
            actual_identities,
            range,
            application_identity
        ) && CFArrayContainsValue(
            actual_identities,
            range,
            security_tool_identity
        );
    }
    if (application_identity != NULL) {
        CFRelease(application_identity);
    }
    if (security_tool_identity != NULL) {
        CFRelease(security_tool_identity);
    }
    CFRelease(actual_identities);
    return result;
}
