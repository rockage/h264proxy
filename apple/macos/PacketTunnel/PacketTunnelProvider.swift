import Foundation
import NetworkExtension
import Network
import os
import Darwin

@objc(PacketTunnelProvider)
final class PacketTunnelProvider: NEPacketTunnelProvider, @unchecked Sendable {
    private let lock = NSLock()
    private var handle: UInt64 = 0
    private var generation: UInt64 = 0
    private var stopping = true
    private var startCompletion: ((Error?) -> Void)?
    private let startupQueue = DispatchQueue(label: "com.roca.h264proxy.startup", qos: .userInitiated)
    private let outputQueue = DispatchQueue(label: "com.roca.h264proxy.output", qos: .userInitiated)
    private var pathMonitor: NWPathMonitor?
    private var previousPath: String?
    private let logger = Logger(subsystem: "com.roca.h264proxy", category: "PacketTunnel")

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        lock.lock()
        generation &+= 1
        let epoch = generation
        stopping = false
        startCompletion = completionHandler
        lock.unlock()
        startupQueue.async { [weak self] in self?.startEngine(epoch: epoch) }
    }

    private func startEngine(epoch: UInt64) {
        do {
            guard let configuration = protocolConfiguration as? NETunnelProviderProtocol,
                  let endpoint = configuration.providerConfiguration?["endpoint"] as? String,
                  let reference = configuration.passwordReference else { throw TunnelError("VPN 配置或密钥引用缺失。") }
            let settings = TunnelConfiguration(endpoint: endpoint)
            try settings.validate()
            let key = try SharedKeychain.read(reference: reference)
            var message: UnsafeMutablePointer<CChar>?
            let started = settings.engineJSON.withCString { json in
                key.withUnsafeBytes { bytes in
                    H264EngineStart(json, bytes.baseAddress!.assumingMemoryBound(to: UInt8.self), Int32(bytes.count), &message)
                }
            }
            guard started != 0 else { throw engineError(message, fallback: "无法创建传输引擎。") }
            lock.lock()
            let keep = !stopping && generation == epoch
            if keep { handle = started }
            lock.unlock()
            guard keep else { H264EngineStop(started); return }
            guard H264EngineWaitReady(started, 30000, &message) == 0 else {
                throw engineError(message, fallback: "隧道连接或 DNS 健康检查失败。")
            }
            guard isCurrent(started, epoch) else { return }
            let network = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: settings.serverIPv4)
            let ipv4 = NEIPv4Settings(addresses: ["198.18.0.1"], subnetMasks: ["255.255.255.252"])
            ipv4.includedRoutes = [.default()]
            ipv4.excludedRoutes = [NEIPv4Route(destinationAddress: settings.serverIPv4, subnetMask: "255.255.255.255")]
            network.ipv4Settings = ipv4
            // IPv6 is captured to prevent silent physical-path bypass. The
            // inherited IPv4 DNS policy suppresses AAAA and IPv6 service hints.
            let ipv6 = NEIPv6Settings(addresses: ["fdfe:dc:264::1"], networkPrefixLengths: [128])
            ipv6.includedRoutes = [.default()]
            network.ipv6Settings = ipv6
            let dns = NEDNSSettings(servers: ["198.18.0.2"])
            dns.matchDomains = [""]
            network.dnsSettings = dns
            network.mtu = 1500
            setTunnelNetworkSettings(network) { [weak self] error in
                guard let self, self.isCurrent(started, epoch) else { return }
                if let error { self.fail(error, epoch: epoch); return }
                self.readPackets(engine: started, epoch: epoch)
                self.outputQueue.async { [weak self] in self?.writePackets(engine: started, epoch: epoch) }
                self.watchPath(engine: started, epoch: epoch)
                self.finishStart(nil, epoch: epoch)
                self.logger.info("Packet tunnel ready using ICE-TCP")
            }
        } catch { fail(error, epoch: epoch) }
    }

    private func engineError(_ pointer: UnsafeMutablePointer<CChar>?, fallback: String) -> Error {
        guard let pointer else { return TunnelError(fallback) }
        defer { H264EngineFree(pointer) }
        return TunnelError(String(cString: pointer))
    }
    private func isCurrent(_ engine: UInt64, _ epoch: UInt64) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return !stopping && handle == engine && generation == epoch
    }
    private func readPackets(engine: UInt64, epoch: UInt64) {
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self, self.isCurrent(engine, epoch) else { return }
            for (packet, family) in zip(packets, protocols) {
                guard family.int32Value == AF_INET || family.int32Value == AF_INET6 else { continue }
                packet.withUnsafeBytes { bytes in
                    guard let address = bytes.baseAddress else { return }
                    _ = H264EngineInject(engine, address, Int32(bytes.count))
                }
            }
            if self.isCurrent(engine, epoch) { self.readPackets(engine: engine, epoch: epoch) }
        }
    }
    private func writePackets(engine: UInt64, epoch: UInt64) {
        var bytes = [UInt8](repeating: 0, count: 65535)
        while isCurrent(engine, epoch) {
            let n = bytes.withUnsafeMutableBytes { buffer in H264EngineRead(engine, buffer.baseAddress, Int32(buffer.count), 1000) }
            if n < 0 {
                if isCurrent(engine, epoch) { fail(TunnelError("传输连接已断开，请重新连接。"), epoch: epoch) }
                return
            }
            if n == 0 { continue }
            guard isCurrent(engine, epoch) else { return }
            let family = bytes[0] >> 4 == 6 ? AF_INET6 : AF_INET
            if !packetFlow.writePackets([Data(bytes.prefix(Int(n)))], withProtocols: [NSNumber(value: family)]) {
                fail(TunnelError("系统虚拟接口写入失败。"), epoch: epoch); return
            }
        }
    }
    private func finishStart(_ error: Error?, epoch: UInt64? = nil) {
        lock.lock()
        if let epoch, generation != epoch { lock.unlock(); return }
        let completion = startCompletion; startCompletion = nil
        lock.unlock()
        completion?(error)
    }
    private func fail(_ error: Error, epoch: UInt64) {
        startupQueue.async { [weak self] in
            guard let self else { return }
            self.lock.lock()
            let current = !self.stopping && self.generation == epoch
            let old = current ? self.handle : 0
            let monitor = current ? self.pathMonitor : nil
            if current { self.stopping = true; self.handle = 0; self.pathMonitor = nil }
            self.lock.unlock()
            guard current else { return }
            monitor?.cancel()
            if old != 0 { H264EngineStop(old) }
            self.logger.error("Packet tunnel failure: \(error.localizedDescription, privacy: .public)")
            self.finishStart(error, epoch: epoch)
            self.cancelTunnelWithError(error)
        }
    }
    private func watchPath(engine: UInt64, epoch: UInt64) {
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { [weak self] path in
            guard let self else { return }
            let summary = "\(path.status)-wifi:\(path.usesInterfaceType(.wifi))-ethernet:\(path.usesInterfaceType(.wiredEthernet))"
            self.lock.lock()
            let current = !self.stopping && self.generation == epoch
            let changed = current && self.previousPath != nil && self.previousPath != summary
            if current { self.previousPath = summary }
            self.lock.unlock()
            if changed { self.logger.notice("Underlying network changed; reconnect explicitly if ICE fails") }
        }
        lock.lock()
        guard !stopping && handle == engine && generation == epoch else { lock.unlock(); return }
        pathMonitor = monitor
        previousPath = nil
        monitor.start(queue: startupQueue)
        lock.unlock()
    }
    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        lock.lock()
        stopping = true
        generation &+= 1
        let old = handle; handle = 0
        let monitor = pathMonitor; pathMonitor = nil
        lock.unlock()
        monitor?.cancel()
        finishStart(TunnelError("启动已取消。"))
        DispatchQueue.global(qos: .userInitiated).async {
            if old != 0 { H264EngineStop(old) }
            completionHandler()
        }
    }
    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)? = nil) {
        lock.lock(); let current = handle; lock.unlock()
        guard messageData == Data("status".utf8), current != 0,
              let pointer = H264EngineStatus(current) else { completionHandler?(nil); return }
        defer { H264EngineFree(pointer) }
        completionHandler?(Data(String(cString: pointer).utf8))
    }
}
