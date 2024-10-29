AS a note I used helm chart for alloy to do the deployment of alloy, then I thought I could use the alloy-values.yaml in order to adjust variables but realized that is not how that works.

So I just edited the configmap it created directly and kept reloading it until I got the config I wanted. 

Otherwise my pipeline looks like this 

main.go builds using docker build. 
main.go tries to use alloy but something might be wrong with the config. Tempo and Loki both are getting http requests instead and that is mostly working. 

I use docker build to create the docker image for georgetestapp-deployment.yaml .

I use the georgetestapp-deployment.yaml in order to modify some environment variables. 

My heavy suspicion is the config.alloy but idk. I put in the auth header since in intro-mltp it mentioned it might be needed but I haven't seen a difference in my tempo config either way. 
