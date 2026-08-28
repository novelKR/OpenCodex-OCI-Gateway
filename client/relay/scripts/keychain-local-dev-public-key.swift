#!/usr/bin/env swift
import CryptoKit
import Foundation
import Security

let requiredPrefix = "opencodex-relay-local-dev-trust-"

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data(("ERROR: \(message)\n").utf8))
    exit(1)
}

func validService(_ service: String) -> Bool {
    let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._-"))
    return service.hasPrefix(requiredPrefix) &&
        service.utf8.count <= 160 &&
        service.unicodeScalars.allSatisfy(allowed.contains)
}

func itemQuery(service: String) -> [String: Any] {
    [
        kSecClass as String: kSecClassGenericPassword,
        kSecAttrService as String: service,
        kSecAttrAccount as String: NSUserName(),
    ]
}

func sha256(_ value: Data) -> String {
    SHA256.hash(data: value).map { String(format: "%02x", $0) }.joined()
}

func keyData(at path: String) -> Data {
    let url = URL(fileURLWithPath: path)
    guard let data = try? Data(contentsOf: url), !data.isEmpty else {
        fail("public key file is unavailable")
    }
    return data
}

let arguments = CommandLine.arguments
guard arguments.count >= 3 else {
    fail("usage: keychain-local-dev-public-key.swift read SERVICE | enroll SERVICE PUBLIC_PEM EXPECTED_SHA256 | replace SERVICE OLD_SHA256 PUBLIC_PEM NEW_SHA256")
}

let command = arguments[1]
let service = arguments[2]
guard validService(service) else {
    fail("Keychain trust service name is unsafe")
}

switch command {
case "read":
    guard arguments.count == 3 else {
        fail("usage: keychain-local-dev-public-key.swift read SERVICE")
    }
    var query = itemQuery(service: service)
    query[kSecMatchLimit as String] = kSecMatchLimitOne
    query[kSecReturnData as String] = true
    var result: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &result)
    guard status == errSecSuccess, let data = result as? Data, !data.isEmpty else {
        fail("Keychain local development trust key is unavailable")
    }
    FileHandle.standardOutput.write(data)

case "enroll":
    guard arguments.count == 5 else {
        fail("usage: keychain-local-dev-public-key.swift enroll SERVICE PUBLIC_PEM EXPECTED_SHA256")
    }
    let data = keyData(at: arguments[3])
    guard sha256(data) == arguments[4] else {
        fail("public key fingerprint does not match the expected value")
    }
    var query = itemQuery(service: service)
    query[kSecValueData as String] = data
    query[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
    let status = SecItemAdd(query as CFDictionary, nil)
    guard status == errSecSuccess else {
        fail("refusing to replace or create an existing Keychain trust key (status \(status))")
    }

case "replace":
    guard arguments.count == 6 else {
        fail("usage: keychain-local-dev-public-key.swift replace SERVICE OLD_SHA256 PUBLIC_PEM NEW_SHA256")
    }
    let oldFingerprint = arguments[3]
    let data = keyData(at: arguments[4])
    let newFingerprint = arguments[5]
    guard sha256(data) == newFingerprint else {
        fail("new public key fingerprint does not match the expected value")
    }
    var query = itemQuery(service: service)
    query[kSecMatchLimit as String] = kSecMatchLimitOne
    query[kSecReturnData as String] = true
    var result: CFTypeRef?
    let readStatus = SecItemCopyMatching(query as CFDictionary, &result)
    guard readStatus == errSecSuccess, let current = result as? Data, sha256(current) == oldFingerprint else {
        fail("current Keychain trust key does not match the expected fingerprint")
    }
    let update: [String: Any] = [
        kSecValueData as String: data,
        kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
    ]
    guard SecItemUpdate(itemQuery(service: service) as CFDictionary, update as CFDictionary) == errSecSuccess else {
        fail("unable to replace Keychain trust key")
    }

default:
    fail("unknown command")
}
