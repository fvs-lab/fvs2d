import Foundation
import FSKit
import Darwin

@main
struct FVSControl {
    static func main() async {
        do {
            let modules = try await FSClient.shared.installedExtensions
            let module = modules.first { $0.bundleIdentifier == "lab.fvs.FVS.FS" }
            var result: [String: Any] = ["registered": module != nil, "enabled": module?.isEnabled ?? false]
            if let module = module { result["path"] = module.url.path }
            let data = try JSONSerialization.data(withJSONObject: result, options: [.sortedKeys])
            FileHandle.standardOutput.write(data)
            FileHandle.standardOutput.write(Data([10]))
        } catch {
            let result = ["error": error.localizedDescription]
            if let data = try? JSONSerialization.data(withJSONObject: result) {
                FileHandle.standardOutput.write(data)
            }
            exit(1)
        }
    }
}
