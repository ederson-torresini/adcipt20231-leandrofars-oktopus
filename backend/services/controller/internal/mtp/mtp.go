// Defines an interface to be implemented by the choosen MTP.
package mtp

import (
	"log"
	"os"
)

/*
	Message Transfer Protocol layer, which can use WebSockets, MQTT, COAP or STOMP; as defined in tr369 protocol.
	It was made thinking in a broker architeture instead of a server-client p2p.
*/
type Broker interface {
	Connect()
	Disconnect()
	Publish(msg []byte, topic, respTopic string, retain bool)
	Subscribe()
	/*
		At request method we're able to send a message to a topic
		and wait until we have a response (in the same topic).
	*/
	//Request(msg []byte, msgType usp_msg.Header_MsgType, pubTopic string, subTopic string)
}

// Not used, since we are using a broker solution with MQTT.
type P2P interface {
}

// Start the service which enable the communication with IoTs (MTP protocol layer).
func MtpService(b Broker, done chan os.Signal) {
	b.Connect()
	go func() {
		for range done {
			b.Disconnect()
			log.Println("Successfully disconnected to broker!")

			// Receives signal and then replicates it to the rest of the app.
			done <- os.Interrupt
		}
	}()
	//b.Subscribe()
}
