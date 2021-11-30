package pkg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type udpConn struct {
	conn *net.UDPConn
	port int
}

// CreateWebRTCConnection function
func CreateWebRTCConnection(ingestionAddress, streamKey, offerStr string) (answer webrtc.SessionDescription, err error) {

	defer func() {
		if e, ok := recover().(error); ok {
			err = e
		}
	}()

	// Create a MediaEngine object to configure the supported codec
	m := webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/VP8", ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}
	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 audio track, and 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	go func(peerConnection *webrtc.PeerConnection) {
		// Create context
		ctx, cancel := context.WithCancel(context.Background())

		// Create a local addr
		var laddr *net.UDPAddr
		if laddr, err = net.ResolveUDPAddr("udp", "127.0.0.1:"); err != nil {
			fmt.Println(err)
			cancel()
		}

		// Prepare udp conns
		udpConns := map[string]*udpConn{
			"audio": {port: 4000},
			"video": {port: 4002},
		}
		for _, c := range udpConns {
			// Create remote addr
			var raddr *net.UDPAddr
			if raddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", c.port)); err != nil {
				fmt.Println(err)
				cancel()
			}

			// Dial udp
			if c.conn, err = net.DialUDP("udp", laddr, raddr); err != nil {
				fmt.Println(err)
				cancel()
			}
			defer func(conn net.PacketConn) {
				if closeErr := conn.Close(); closeErr != nil {
					fmt.Println(closeErr)
				}
			}(c.conn)
		}

		streamURL := fmt.Sprintf("%s/%s", ingestionAddress, streamKey)
		startFFmpeg(ctx, streamURL)

		// Set a handler for when a new remote track starts, this handler will forward data to
		// our UDP listeners.
		// In your application this is where you would handle/process audio/video
		peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

			// Retrieve udp connection
			c, ok := udpConns[track.Kind().String()]
			if !ok {
				return
			}

			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
			go func() {
				ticker := time.NewTicker(time.Second * 2)
				for range ticker.C {
					if rtcpErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); rtcpErr != nil {
						fmt.Println(rtcpErr)
					}
					if ctx.Err() == context.Canceled {
						break
					}
				}
			}()

			b := make([]byte, 1500)
			for {
				// Read
				n, _, readErr := track.Read(b)
				if readErr != nil {
					fmt.Println(readErr)
				}

				// Write
				if _, err = c.conn.Write(b[:n]); err != nil {
					fmt.Println(err)
					if ctx.Err() == context.Canceled {
						break
					}
				}
			}
		})

		// in a production application you can either trickle ICE by exchanging ICE Candidates via OnICECandidate
		// or disable trickle by waiting until ice gathering is complete before sending out the peerConnection answer (LocalDescription)
		peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			fmt.Println(candidate)
		})

		// Set the handler for ICE connection state
		// This will notify you when the peer has connected/disconnected
		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Printf("Connection State has changed %s \n", connectionState.String())

			if connectionState == webrtc.ICEConnectionStateConnected {
				fmt.Println("ICE connection was successful")
			} else if connectionState == webrtc.ICEConnectionStateFailed ||
				connectionState == webrtc.ICEConnectionStateDisconnected {
				cancel()
			}
		})

		// Wait for context to be done
		<-ctx.Done()
		peerConnection.Close()

	}(peerConnection)

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	err = json.Unmarshal([]byte(offerStr), &offer)
	if err != nil {
		panic(err)
	}

	// Set the remote SessionDescription
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	// Create answer
	answer, err = peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Sets the LocalDescription, and starts our UDP listeners
	// to disable ICE trickle don't send the local description immediately
	// instead wait until ice gathering is complete before sending it
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	return
}

func startFFmpeg(ctx context.Context, streamURL string) {
	// Create a ffmpeg process that consumes MKV via stdin, and broadcasts out to Stream URL
	ffmpeg := exec.CommandContext(ctx, "ffmpeg", "-protocol_whitelist", "file,udp,rtp", "-i", "rtp-forwarder.sdp", "-c:v", "copy", "-c:a", "aac", "-f", "flv", "-strict", "-2", streamURL) //nolint
	ffmpeg.StdinPipe()
	ffmpegOut, _ := ffmpeg.StderrPipe()
	if err := ffmpeg.Start(); err != nil {
		panic(err)
	}

	go func() {
		scanner := bufio.NewScanner(ffmpegOut)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
			if ctx.Err() == context.Canceled {
				break
			}
		}
	}()
}
