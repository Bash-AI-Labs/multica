FROM node:22.23.4-alpine

RUN apk add --no-cache openssl && mkdir /tls && chown node:node /tls
WORKDIR /app
COPY handler.mjs entrypoint.sh ./

USER node
EXPOSE 8787
ENTRYPOINT ["sh", "/app/entrypoint.sh"]
