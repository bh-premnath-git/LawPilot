import { startTelemetry, shutdownTelemetry, withHttpSpan } from '@lawpilot/telemetry';
import 'reflect-metadata';
import { Module, Controller, Get, Post, Param, Req, Body, Res, HttpException, HttpCode, Injectable, OnApplicationShutdown } from '@nestjs/common';
import { NestFactory } from '@nestjs/core';
import { Request, Response, json } from 'express';
import { randomUUID } from 'node:crypto';
import { AuthService } from './auth.service';
import { DbService } from './db.service';
import {ExtensionsController,ProviderWebhookController} from './integrations/extensions.controller';
import {IntegrationController,IntegrationClient,LegalReviewController} from './integrations/integrations.controller';
import {ContextController,ContextClient} from './context/context.controller';
import {CalendarController,CalendarClient} from './calendar/calendar.controller';
import { ExecutionsService } from './executions.service';
import { AccessService } from './access.service';

startTelemetry('platform-api');
function invalid(status:number, message:string):never{throw new HttpException({error:message},status);}

@Controller('v1')
export class ApiController {
  constructor(private auth:AuthService,private access:AccessService,private db:DbService,private executions:ExecutionsService){}
  @Get('healthz') health(){return {service:'platform-api',status:'up',liveProvider:'not-configured'};}

  @Get('readyz')
  async ready(){
    try {await this.db.ready();return {service:'platform-api',status:'ready'};}
    catch {throw new HttpException({error:'DATABASE_NOT_READY'},503);}
  }

  @Get('tenants/:tenantId/matters/:matterId')
  async matter(@Param('tenantId') tenantId:string,@Param('matterId') matterId:string,@Req() req:Request){
    const principal=await this.auth.subject(req.headers.authorization);
    return this.access.withAccess(tenantId,principal,undefined,matterId,async c=>{
      const r=await c.query('SELECT id,title,status,revision FROM matters WHERE tenant_id=$1 AND id=$2',[tenantId,matterId]);
      if(!r.rowCount) invalid(404,'Matter not found');
      return {matter:r.rows[0]};
    });
  }

  @Post(['tenants/:tenantId/executions','tenants/:tenantId/ai/execute'])
  @HttpCode(202)
  async execute(@Param('tenantId') tenant:string,@Req() req:Request,@Body() raw:unknown,@Res({passthrough:true}) res:Response){
    const result=await this.executions.accept(tenant,await this.auth.subject(req.headers.authorization),raw);
    res.setHeader('Location',result.statusUrl);return result;
  }
  @Get('tenants/:tenantId/executions/:id')
  async status(@Param('tenantId') tenant:string,@Param('id') id:string,@Req() req:Request){
    return this.executions.inspect(tenant,await this.auth.subject(req.headers.authorization),id);
  }
  @Post('tenants/:tenantId/executions/:id/cancel')
  @HttpCode(202)
  async cancel(@Param('tenantId') tenant:string,@Param('id') id:string,@Req() req:Request){
    return this.executions.inspect(tenant,await this.auth.subject(req.headers.authorization),id,'cancel');
  }
  @Post('tenants/:tenantId/executions/:id/dispatch/retry')
  @HttpCode(202)
  async retry(@Param('tenantId') tenant:string,@Param('id') id:string,@Req() req:Request){
    return this.executions.inspect(tenant,await this.auth.subject(req.headers.authorization),id,'retry');
  }

}

@Injectable()
class TelemetryLifecycle implements OnApplicationShutdown {
  async onApplicationShutdown(): Promise<void> { await shutdownTelemetry(); }
}

@Module({controllers:[ApiController,CalendarController,ContextController,IntegrationController,LegalReviewController,ExtensionsController,ProviderWebhookController],providers:[AuthService,DbService,AccessService,ExecutionsService,CalendarClient,ContextClient,IntegrationClient,TelemetryLifecycle]})
class AppModule{}
async function bootstrap(){
  if(!process.env.KEYCLOAK_ISSUER||!process.env.KEYCLOAK_AUDIENCE||(!process.env.DATABASE_URL&&!process.env.DATABASE_URL_FILE)){throw new Error('Missing required auth/DB env');}
  const app=await NestFactory.create(AppModule,{cors:false,bodyParser:false});
  app.use((req: any,res: any,next: () => void)=>withHttpSpan('platform.request',req,res,next));
  app.use((req: any, res: any, next: () => void) => {
    res.setHeader('Cache-Control','no-store');
    res.setHeader('X-Content-Type-Options','nosniff');
    const requestId=randomUUID();res.setHeader('X-Request-ID',requestId);
    Object.defineProperty(req,'lawpilotRequestId',{value:requestId,writable:false});
    next();
  });
  app.use('/v1/provider-webhooks',json({limit:'64kb',strict:true,verify:(req:any,_res:any,buffer:Buffer)=>{req.rawBody=Buffer.from(buffer)}}));
  app.use(json({limit: '32kb', strict: true}));
  app.enableShutdownHooks();
  await app.listen(Number(process.env.API_PORT||3000),'0.0.0.0');
}
if (require.main === module) bootstrap().catch(e=>{console.error(e);process.exit(1)});
