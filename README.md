WStunnel - Web Sockets Tunnel
=============================

WStunnel creates an HTTPS tunnel that can connect servers sitting
behind an HTTP proxy and firewall to clients on the internet. It differs from many other projects
by handling many concurrent tunnels allowing a central client (or set of clients) to make requests
to many servers sitting behind firewalls. Each client/server pair are joined through a rendez-vous token.

At the application level the
situation is as follows, an HTTP client wants to make request to an HTTP server behind a
firewall and the ingress is blocked by a firewall:

    HTTP-client ===> ||firewall|| ===> HTTP-server

The WStunnel app implements a tunnel through the firewall. The assumption is that the
WStunnel client app running on the HTTP-server box can make outbound HTTPS requests. In the end
there are 4 components running on 3 servers involved:
 - the http-client application on a client box initiates HTTP requests
 - the http-server application on a server box behind a firewall handles the HTTP requests
 - the WStunnel server application on a 3rd box near the http-client intercepts the http-client's
   requests in order to tunnel them through (it acts as a surrogate "server" to the http-client)
 - the WStunnel client application on the server box hands the http requests to the local
   http-server app (it acts as a "client" to the http-server)
The result looks something like this:

````
    HTTP-client ==>\                      /===> HTTP-server
                   |                      |
                   \----------------------/
               WStunsrv <===tunnel==== WStuncli
````

But this is not the full picture. Many WStunnel clients can connect to the same server and
many http-clients can make requests. The rendez-vous between these is made using secret
tokens that are registered by the WStunnel client. The steps are as follows:
 - WStunnel client is initialized with a token, which typically is a sizeable random string,
   and the hostname of the WStunnel server to connect to
 - WStunnel client connects to the WStunnel server using WSS or HTTPS and verifies the
   hostname-certificate match
 - WStunnel client announces its token to the WStunnel server
 - HTTP-client makes an HTTP request to WStunnel server with a std URI and a header
   containing the secret token
 - WStunnel server forwards the request through the tunnel to WStunnel client
 - WStunnel client receives the request and issues the request to the local server
 - WStunnel client receives the HTTP reqponse and forwards that back through the tunnel, where
   WStunnel server receives it and hands it back to HTTP-client on the still-open original
   HTTP request

In addition to the above functionality, wstunnel does some queuing in
order to handle situations where the tunnel is momentarily not open. However, during such
queing any HTTP connections to the HTTP-server/client remain open, i.e., they are not
made aware of the queueing happening.

The implementation of the actual tunnel is intended to support two methods (but only the
first is currently implemented).  
The preferred high performance method is websockets: the WStunnel client opens a secure
websockets connection to WStunnel server using the HTTP CONNECT proxy traversal connection
upgrade if necessary and the two ends use this connection as a persistent bi-directional
tunnel.  
The second (Not yet implemented!) lower performance method is to use HTTPS long-poll where the WStunnel client
makes requests to the server to shuffle data back and forth in the request and response
bodies of these requests.

Pre-requisites
---------------
- JDK / JRE 8 or above
- make

Getting Started
---------------
#### Server
1. `make version` - Create Version go file
2. `./gradlew -b server.gradle build` - Create Server binary
3. `cd build/server/`
4. `./wstunnel -port 7080`

#### Client
1. `make version` - Create Version go file
2. `./gradlew -b client.gradle build` - Create Client binary

TODO
----
* [ ] Remove `make` dependency
* [ ] Version label from Pipeline
