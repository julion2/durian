//
//  KeychainHelper.swift
//  Durian
//
//  Keychain password retrieval utility
//

import Foundation
import Security

struct KeychainHelper {
    static func retrievePassword(service: String, account: String) -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne
        ]

        var result: AnyObject?
        let status = SecItemCopyMatching(query as CFDictionary, &result)

        if status == errSecSuccess,
           let data = result as? Data,
           let password = String(data: data, encoding: .utf8)
        {
            return password
        } else {
            Log.error("KEYCHAIN", "Failed to retrieve password from keychain for \(account): \(status)")
            return nil
        }
    }
}
